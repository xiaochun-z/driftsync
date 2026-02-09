package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaochun-z/driftsync/internal/auth"
	"github.com/xiaochun-z/driftsync/internal/config"
	"github.com/xiaochun-z/driftsync/internal/graph"
	"github.com/xiaochun-z/driftsync/internal/store"
	_ "modernc.org/sqlite"
)

// 配置
const (
	TestRootDir  = "test_workspace"
	LocalSyncDir = "local_files"
	TestScopeDir = "DriftSync_E2E_Test" // 将测试限制在此子目录中
	ConfigName   = "config_test.yaml"
	BinaryName   = "driftsync_test.exe"
	LogPrefix    = "[E2E]"
)

var (
	// 全局路径
	absRoot      string
	absLocalPath string
	absTestScope string
	absBinary    string
	absConfig    string

	// 自动化测试客户端
	cloudClient *graph.Client
	ctx         context.Context
)

func main() {
	ctx = context.Background()
	setupEnvironment()
	compileBinary()

	fmt.Println("\n🚀 Driftsync 自动化 E2E 测试套件启动")
	fmt.Printf("📂 测试根目录: %s\n", absRoot)
	fmt.Printf("🔒 测试范围 (Selective Sync): /%s\n", TestScopeDir)

	// 初始化云端客户端 (复用本地 Token)
	// 注意：首次运行如果未登录，initCloudClient 会失败，需要先跑一次 sync 让用户登录。
	// 这里我们先尝试跑一次 Sync (空跑), 如果需要登录会提示。
	fmt.Println("🔑 正在检查登录状态...")
	runSync()

	if err := initCloudClient(); err != nil {
		fatal(fmt.Sprintf("无法初始化云端客户端: %v\n请确保第一次运行时在终端完成了登录流程。", err))
	}

	// 确保云端环境干净 (Closed Loop Start)
	fmt.Println("🧹 正在清理云端测试环境...")
	cleanCloudScope()
	// 确保结束后也清理 (Closed Loop End)
	defer func() {
		fmt.Println("\n🧹 测试结束，正在清理云端残留...")
		cleanCloudScope()
	}()

	fmt.Println("✅ 环境就绪，开始执行测试用例...")

	// === 测试用例集 ===

	// Case 1: 基础上传
	runTest("B01: 本地新建 -> 云端上传", func() {
		createFile("hello.txt", "Hello World v1")
		runSync()
		verifyLocalFile("hello.txt", "Hello World v1") // 本地文件应保持
		verifyCloudFile("hello.txt", "Hello World v1") // 云端应存在
	})

	// Case 2: 云端下载
	runTest("B02: 云端新建 -> 本地下载", func() {
		// 自动化：直接调用 API 在云端创建文件
		createCloudFile("cloud_new.txt", "Created by E2E automation")

		runSync()
		verifyLocalFile("cloud_new.txt", "Created by E2E automation")
	})

	// Case 3: 修改传播 (本地 -> 云端)
	runTest("B03: 本地修改 -> 云端更新", func() {
		createFile("hello.txt", "Hello World v2 (Modified)")
		runSync()
		verifyCloudFile("hello.txt", "Hello World v2 (Modified)")
	})

	// Case 4: 嵌套目录结构 (新增)
	runTest("S01: 嵌套目录结构同步", func() {
		// Local -> Cloud
		createFile("deep/nested/folder/struct.txt", "Deep Data")
		runSync()
		verifyCloudFile("deep/nested/folder/struct.txt", "Deep Data")

		// Cloud -> Local
		createCloudFile("deep/nested/folder/cloud_deep.txt", "Cloud Deep Data")
		runSync()
		verifyLocalFile("deep/nested/folder/cloud_deep.txt", "Cloud Deep Data")
	})

	// Case 5: 重命名 (新增)
	runTest("S02: 本地重命名 -> 云端同步", func() {
		// 准备环境
		createFile("rename_src.txt", "Move Me")
		runSync()
		verifyCloudFile("rename_src.txt", "Move Me")

		// 执行重命名
		src := filepath.Join(absTestScope, "rename_src.txt")
		dst := filepath.Join(absTestScope, "rename_dst.txt")
		os.Rename(src, dst)

		runSync()

		verifyCloudNotExist("rename_src.txt")
		verifyCloudFile("rename_dst.txt", "Move Me")
	})

	// Case 6: 排除规则 (新增)
	runTest("S03: 排除规则验证 (*.ignore)", func() {
		createFile("secret.ignore", "This should not upload")
		createFile("allowed.txt", "This should upload")

		runSync()

		verifyCloudNotExist("secret.ignore")
		verifyCloudFile("allowed.txt", "This should upload")
	})

	// Case 7: 云端删除同步
	runTest("B06: 云端删除 -> 本地删除", func() {
		// 先确保文件存在
		createCloudFile("to_be_deleted.txt", "Bye Bye")
		runSync()
		verifyLocalFile("to_be_deleted.txt", "Bye Bye")

		// 云端删除
		deleteCloudFile("to_be_deleted.txt")
		runSync()

		// 验证本地删除
		verifyFileDeleted("to_be_deleted.txt")
	})

	// Case 8: 本地删除同步
	runTest("B05: 本地删除 -> 云端删除", func() {
		createFile("local_del.txt", "Delete me")
		runSync()
		verifyCloudFile("local_del.txt", "Delete me")

		removeFile("local_del.txt")
		runSync()

		verifyCloudNotExist("local_del.txt")
	})

	// Case 9: 冲突处理
	runTest("C01: 冲突处理 (保留两者)", func() {
		createFile("conflict.txt", "Base Version")
		runSync()

		// 制造冲突：云端更新 & 本地更新
		updateCloudFile("conflict.txt", "Cloud Version")
		createFile("conflict.txt", "Local Version")

		runSync()

		// 验证: 应该有两个文件
		entries, _ := os.ReadDir(absTestScope)
		conflictCount := 0
		for _, e := range entries {
			if strings.Contains(e.Name(), "conflict") {
				conflictCount++
				fmt.Printf("   🔍 发现文件: %s\n", e.Name())
			}
		}
		if conflictCount < 2 {
			fatal("FAIL: 未产生冲突文件 (应该保留两个版本)")
		}
	})

	// Case 10: 安全性 - 垃圾文件
	runTest("E03: 安全性 - 忽略回收站和系统文件", func() {
		createFile(".DS_Store", "garbage")

		// 模拟 Trash
		trashPath := filepath.Join(absTestScope, ".driftsync_trash")
		os.MkdirAll(trashPath, 0755)
		os.WriteFile(filepath.Join(trashPath, "bad_file.txt"), []byte("should not upload"), 0644)

		output := runSync()

		if strings.Contains(output, ".DS_Store") || strings.Contains(output, "bad_file.txt") {
			fatal("FAIL: 垃圾文件被尝试上传了！")
		}
		verifyCloudNotExist(".DS_Store")
		verifyCloudNotExist(".driftsync_trash/bad_file.txt")
	})

	fmt.Println("\n✅✅✅ 所有测试用例执行完毕！OK！")
}

// --- 核心辅助函数 ---

func setupEnvironment() {
	cwd, _ := os.Getwd()
	absRoot = filepath.Join(cwd, TestRootDir)
	absLocalPath = filepath.Join(absRoot, LocalSyncDir)
	absTestScope = filepath.Join(absLocalPath, TestScopeDir)
	absBinary = filepath.Join(cwd, BinaryName)
	absConfig = filepath.Join(absRoot, ConfigName)
	dbPath := filepath.Join(absRoot, "driftsync.db")

	os.RemoveAll(absLocalPath)
	os.MkdirAll(absTestScope, 0755)

	// 清理 DB 状态但保留 Token
	if _, err := os.Stat(dbPath); err == nil {
		db, err := sql.Open("sqlite", dbPath)
		if err == nil {
			_, _ = db.Exec("DELETE FROM items")
			_, _ = db.Exec("DELETE FROM meta")
			db.Close()
		}
	} else {
		os.MkdirAll(absRoot, 0755)
	}
	os.Remove(absBinary)
}

func compileBinary() {
	fmt.Print("🔨 正在编译最新代码... ")
	cmd := exec.Command("go", "build", "-ldflags", "-s -w", "-o", absBinary, "../../cmd/driftsync")
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatalf("编译失败:\n%s", string(out))
	}
	fmt.Println("完成。")
}

func initCloudClient() error {
	// 读取测试目录下的 DB
	dbPath := filepath.Join(absRoot, "driftsync.db")
	db, err := store.Open(dbPath)
	if err != nil {
		return err
	}

	// 读取 Config 获取 ClientID
	cfg, err := config.Load(absConfig)
	if err != nil {
		return err
	}

	tokenStore := store.NewTokenStore(db)

	// 这里直接使用 AuthorizedClient，假设 Token 已经在 DB 里了
	// 我们复用 main app 的 auth 逻辑
	authClient := auth.NewDeviceCodeClient(cfg.Tenant, cfg.ClientID, tokenStore)

	// 验证 Token 是否可用 (不进行交互式登录，仅检查)
	httpClient := authClient.AuthorizedClient(ctx)

	// 尝试做一次简单请求来验证
	g := graph.NewClient(httpClient)
	_, err = g.GetItemByPath(ctx, "/") // Check root
	if err != nil && strings.Contains(err.Error(), "401") {
		return fmt.Errorf("Token 失效或不存在，请先手动运行一次程序完成登录")
	}

	cloudClient = g
	return nil
}

// 递归删除云端测试目录
func cleanCloudScope() {
	rel := TestScopeDir
	// 尝试获取，如果不存在则无需删除
	item, err := cloudClient.GetItemByPath(ctx, "/"+rel)
	if err != nil || item == nil {
		return
	}

	// 直接删除整个文件夹
	err = cloudClient.DeleteByPath(ctx, "/"+rel)
	if err != nil {
		fmt.Printf("⚠️  Cloud Clean Warning: %v\n", err)
	} else {
		fmt.Println("   ✨ Cloud Cleaned.")
	}
	// 等待 API 传播
	time.Sleep(1 * time.Second)
}

func createCloudFile(relPath string, content string) {
	fullRel := filepath.Join(TestScopeDir, relPath)
	fullRel = filepath.ToSlash(fullRel)

	// 创建临时本地文件用于上传
	tmpPath := filepath.Join(os.TempDir(), "driftsync_e2e_upload.tmp")
	os.WriteFile(tmpPath, []byte(content), 0644)
	defer os.Remove(tmpPath)

	_, err := cloudClient.UploadSmall(ctx, "/"+fullRel, tmpPath, "")
	if err != nil {
		fatal(fmt.Sprintf("Cloud Create Failed [%s]: %v", relPath, err))
	}
	fmt.Printf("   ☁️  云端创建: %s\n", relPath)
	time.Sleep(1 * time.Second) // 等待云端索引
}

func updateCloudFile(relPath, content string) {
	createCloudFile(relPath, content) // Update is same as create for overwrite
}

func deleteCloudFile(relPath string) {
	fullRel := filepath.Join(TestScopeDir, relPath)
	fullRel = filepath.ToSlash(fullRel)
	err := cloudClient.DeleteByPath(ctx, "/"+fullRel)
	if err != nil {
		fatal(fmt.Sprintf("Cloud Delete Failed [%s]: %v", relPath, err))
	}
	fmt.Printf("   ☁️  云端删除: %s\n", relPath)
}

func verifyCloudFile(relPath, expectedContent string) {
	fullRel := filepath.Join(TestScopeDir, relPath)
	fullRel = filepath.ToSlash(fullRel)

	// 1. Get Item
	item, err := cloudClient.GetItemByPath(ctx, "/"+fullRel)
	if err != nil {
		fatal(fmt.Sprintf("Cloud Verify Failed: File not found [%s]: %v", relPath, err))
	}

	// 2. Download to temp to verify content
	tmpPath := filepath.Join(os.TempDir(), "driftsync_e2e_verify.tmp")
	os.Remove(tmpPath)
	err = cloudClient.DownloadTo(ctx, item.ID, tmpPath)
	if err != nil {
		fatal(fmt.Sprintf("Cloud Verify Download Failed: %v", err))
	}

	content, _ := os.ReadFile(tmpPath)
	if string(content) != expectedContent {
		fatal(fmt.Sprintf("Cloud Verify Content Mismatch. Expected '%s', Got '%s'", expectedContent, string(content)))
	}
	fmt.Printf("   ✅ 云端验证通过: %s\n", relPath)
}

func verifyCloudNotExist(relPath string) {
	fullRel := filepath.Join(TestScopeDir, relPath)
	fullRel = filepath.ToSlash(fullRel)

	_, err := cloudClient.GetItemByPath(ctx, "/"+fullRel)
	if err == nil {
		fatal(fmt.Sprintf("Cloud Verify Failed: File exists but should NOT [%s]", relPath))
	}
	fmt.Printf("   ✅ 云端验证通过 (文件不存在): %s\n", relPath)
}

func runTest(name string, fn func()) {
	fmt.Printf("\n------------------------------------------------\n")
	fmt.Printf("🧪 TEST: %s\n", name)
	fmt.Printf("------------------------------------------------\n")
	fn()
	fmt.Printf("✨ PASS\n")
}

func runSync() string {
	fmt.Println("   🔄 正在执行同步...")
	cmd := exec.Command(absBinary, "--config", ConfigName)
	cmd.Dir = absRoot
	var outBuf strings.Builder
	cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &outBuf)

	err := cmd.Run()
	output := outBuf.String()
	if err != nil {
		// 如果是因为需要登录而退出，不直接 fail，交给后续检查
		if !strings.Contains(output, "Go to") {
			fmt.Printf("\n❌ 同步进程出错 (Exit Code != 0)\n")
		}
	}
	return output
}

func createFile(relPath, content string) {
	p := filepath.Join(absTestScope, relPath)
	os.MkdirAll(filepath.Dir(p), 0755)
	os.WriteFile(p, []byte(content), 0644)
	fmt.Printf("   📝 本地写入: %s\n", relPath)
}

func removeFile(relPath string) {
	p := filepath.Join(absTestScope, relPath)
	os.Remove(p)
	fmt.Printf("   🗑️  本地删除: %s\n", relPath)
}

func verifyLocalFile(relPath, expectedContent string) {
	p := filepath.Join(absTestScope, relPath)
	b, err := os.ReadFile(p)
	if err != nil {
		fatal(fmt.Sprintf("Local Verify Failed: File missing [%s]", relPath))
	}
	if string(b) != expectedContent {
		fatal(fmt.Sprintf("Local Content Mismatch. Want '%s', Got '%s'", expectedContent, string(b)))
	}
}

func verifyFileDeleted(relPath string) {
	p := filepath.Join(absTestScope, relPath)
	if _, err := os.Stat(p); err == nil {
		fatal(fmt.Sprintf("Local Verify Failed: File exists but should be deleted [%s]", relPath))
	}
}

func fatal(msg string) {
	fmt.Printf("\n🔴 TEST FAILED: %s\n", msg)
	os.Exit(1)
}
