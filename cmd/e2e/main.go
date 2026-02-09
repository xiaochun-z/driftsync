package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// 配置
const (
	TestRootDir   = "test_workspace"
	LocalSyncDir  = "local_files"
	TestScopeDir  = "DriftSync_E2E_Test" // 将测试限制在此子目录中
	ConfigName    = "config_test.yaml"
	BinaryName    = "driftsync_test.exe"
	LogPrefix     = "[E2E]"
)

var (
	// 全局路径
	absRoot      string
	absLocalPath string
	absTestScope string
	absBinary    string
	absConfig    string
)

func main() {
	setupEnvironment()
	compileBinary()

	fmt.Println("\n🚀 Driftsync 交互式 E2E 测试套件启动")
	fmt.Printf("📂 测试根目录: %s\n", absRoot)
	fmt.Printf("🔒 测试范围 (Selective Sync): /%s\n", TestScopeDir)
	fmt.Println("⚠️  注意: 测试将自动在你的 OneDrive 创建/使用 '/DriftSync_E2E_Test' 文件夹。")
	pause("按 Enter 开始测试...")

	// === 测试用例集 ===

	// Case 1: 基础上传
	runTest("B01: 本地新建 -> 云端上传", func() {
		createFile("hello.txt", "Hello World v1")
		runSync()
		verifyLocalFile("hello.txt", "Hello World v1")
		manualCheck("请检查 OneDrive 云端 '/DriftSync_E2E_Test' 目录，是否存在 'hello.txt'？")
	})

	// Case 2: 云端下载 (模拟)
	runTest("B02: 云端新建 -> 本地下载", func() {
		manualCheck("请在 OneDrive 云端 '/DriftSync_E2E_Test' 目录新建一个文件 'cloud_new.txt'。")
		runSync()
		if _, err := os.Stat(filepath.Join(absTestScope, "cloud_new.txt")); os.IsNotExist(err) {
			fatal("FAIL: 本地未发现 'cloud_new.txt'")
		}
	})

	// Case 3: 修改传播 (本地 -> 云端)
	runTest("B03: 本地修改 -> 云端更新", func() {
		createFile("hello.txt", "Hello World v2 (Modified)")
		runSync()
		verifyLocalFile("hello.txt", "Hello World v2 (Modified)")
		manualCheck("请检查 OneDrive 云端 '/DriftSync_E2E_Test/hello.txt' 内容是否已更新？")
	})

	// Case 4: 云端删除同步 (关键修复验证)
	runTest("B06: 云端删除 -> 本地删除 (Bug修复验证)", func() {
		manualCheck("请在 OneDrive 云端 '/DriftSync_E2E_Test' 目录 **删除** 'hello.txt'。")
		runSync()
		verifyFileDeleted("hello.txt")
	})

	// Case 5: 本地删除同步
	runTest("B05: 本地删除 -> 云端删除", func() {
		// 先创建一个要删的文件
		createFile("to_delete.txt", "delete me")
		runSync()
		manualCheck("请确认云端已存在 'to_delete.txt'。")

		// 执行删除
		removeFile("to_delete.txt")
		runSync()
		manualCheck("请检查 OneDrive 云端 'to_delete.txt' 是否已被删除？")
	})

	// Case 6: 冲突处理 (双向修改)
	runTest("C01: 冲突处理 (保留两者)", func() {
		createFile("conflict.txt", "Base Version")
		runSync()

		// 制造冲突
		createFile("conflict.txt", "Local Version")
		manualCheck("请在 OneDrive 云端修改 '/DriftSync_E2E_Test/conflict.txt' 的内容为 'Cloud Version'。")

		runSync()

		// 验证
		// 应该有两个文件：conflict.txt (可能是云端版) 和 conflict.local-conflict...txt
		entries, _ := os.ReadDir(absTestScope)
		conflictCount := 0
		for _, e := range entries {
			if strings.Contains(e.Name(), "conflict") {
				conflictCount++
				fmt.Printf("   发现文件: %s\n", e.Name())
			}
		}
		if conflictCount < 2 {
			fatal("FAIL: 未产生冲突文件 (应该保留两个版本)")
		}
	})

	// Case 7: 安全性 - 忽略垃圾文件
	// 注意：由于启用了 Selective Sync，垃圾文件不仅要过 Scanner 检查，还会过 Sync Filter 检查。
	// 为了测试 Scanner 安全性，我们尝试在 TestScope 内部放垃圾文件。
	runTest("E03: 安全性 - 忽略回收站和系统文件", func() {
		createFile(".DS_Store", "garbage")
		
		// 模拟 Trash (Trash 默认在 LocalPath 根目录，但也可能出现在子目录如果用户误操作)
		// 我们在 scope 内创建一个伪造的 trash 目录看是否被上传
		trashPath := filepath.Join(absTestScope, ".driftsync_trash")
		os.MkdirAll(trashPath, 0755)
		os.WriteFile(filepath.Join(trashPath, "bad_file.txt"), []byte("should not upload"), 0644)

		output := runSync()

		if strings.Contains(output, ".DS_Store") || strings.Contains(output, "bad_file.txt") {
			fatal("FAIL: 垃圾文件被尝试上传了！查看日志输出。")
		}
		fmt.Println("   PASS: 垃圾文件未出现在上传列表中。")
	})

	fmt.Println("\n✅✅✅ 所有测试用例执行完毕！系统稳健！")
	cleanup()
}

// --- 辅助函数 ---

func setupEnvironment() {
	cwd, _ := os.Getwd()
	absRoot = filepath.Join(cwd, TestRootDir)
	absLocalPath = filepath.Join(absRoot, LocalSyncDir)
	absTestScope = filepath.Join(absLocalPath, TestScopeDir)
	absBinary = filepath.Join(cwd, BinaryName)
	absConfig = filepath.Join(absRoot, ConfigName)
	dbPath := filepath.Join(absRoot, "driftsync.db")

	// 1. 清理本地同步文件夹内容，但不删除根目录（保留 DB）
	os.RemoveAll(absLocalPath)
	os.MkdirAll(absTestScope, 0755)

	// 2. 如果存在数据库，清理同步状态（items, meta），但保留 tokens
	if _, err := os.Stat(dbPath); err == nil {
		db, err := sql.Open("sqlite", dbPath)
		if err == nil {
			// 清理文件追踪记录和增量令牌，使每次测试都像“第一次同步”但已登录
			_, _ = db.Exec("DELETE FROM items")
			_, _ = db.Exec("DELETE FROM meta")
			db.Close()
			fmt.Println("   ♻️  已清理旧同步记录，保留登录 Token。")
		}
	} else {
		os.MkdirAll(absRoot, 0755)
	}

	// 3. 始终重新编译二进制以包含最新逻辑
	os.Remove(absBinary)

	// 生成测试配置 (启用 Selective Sync)
	// 注意：sync.include 使用正斜杠
	// 已更新为 consumers 租户和示例中的 Client ID
	configContent := fmt.Sprintf(`
tenant: consumers
client_id: 3ae224c9-f16c-42d1-bf5d-44151f2b99fa
local_path: "%s"
download_from_cloud: true
upload_from_local: true
log:
  verbose: true
sync:
  include:
    - "/%s"
`, strings.ReplaceAll(absLocalPath, "\\", "\\\\"), TestScopeDir)

	os.WriteFile(absConfig, []byte(configContent), 0644)
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

func runTest(name string, fn func()) {
	fmt.Printf("\n------------------------------------------------\n")
	fmt.Printf("🧪 TEST: %s\n", name)
	fmt.Printf("------------------------------------------------\n")
	fn()
	fmt.Printf("✨ PASS\n")
}

func runSync() string {
	fmt.Println("   🔄 正在执行同步 (请留意是否有登录提示)...")
	// 必须把工作目录设为 Config 所在目录，否则 db 会生成错位置
	cmd := exec.Command(absBinary, "--config", ConfigName)
	cmd.Dir = absRoot
	
	// FIX: 直接对接标准输出，以便用户能看到 Device Code 登录提示
	// 同时使用 builder 捕获输出用于后续的断言分析
	var outBuf strings.Builder
	cmd.Stdout = io.MultiWriter(os.Stdout, &outBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &outBuf)

	err := cmd.Run()
	output := outBuf.String()

	if err != nil {
		fmt.Printf("\n❌ 同步进程出错 (Exit Code != 0)\n")
		fatal("Sync execution failed")
	}

	// 简单的日志分析
	if strings.Contains(output, "panic") {
		fatal("CRITICAL: 程序发生了 Panic!")
	}
	return output
}

func createFile(relPath, content string) {
	p := filepath.Join(absTestScope, relPath)
	os.WriteFile(p, []byte(content), 0644)
	fmt.Printf("   📝 本地写入 (Scope): %s\n", relPath)
}

func removeFile(relPath string) {
	p := filepath.Join(absTestScope, relPath)
	os.Remove(p)
	fmt.Printf("   🗑️  本地删除 (Scope): %s\n", relPath)
}

func verifyLocalFile(relPath, expectedContent string) {
	p := filepath.Join(absTestScope, relPath)
	b, err := os.ReadFile(p)
	if err != nil {
		fatal(fmt.Sprintf("FAIL: 文件丢失: %s", relPath))
	}
	if string(b) != expectedContent {
		fatal(fmt.Sprintf("FAIL: 内容不匹配. 期望 '%s', 实际 '%s'", expectedContent, string(b)))
	}
}

func verifyFileDeleted(relPath string) {
	p := filepath.Join(absTestScope, relPath)
	if _, err := os.Stat(p); err == nil {
		fatal(fmt.Sprintf("FAIL: 文件本该被删除，但依然存在: %s", relPath))
	}
}

func manualCheck(msg string) {
	fmt.Printf("\n👉 [人工操作] %s\n", msg)
	pause("   操作完成后，按 Enter 继续...")
}

func pause(msg string) {
	fmt.Print(msg)
	bufio.NewReader(os.Stdin).ReadBytes('\n')
}

func fatal(msg string) {
	fmt.Printf("\n🔴 TEST FAILED: %s\n", msg)
	os.Exit(1)
}

func cleanup() {
	// 可选：测试完保留现场以便查看，或者打开下面的注释自动清理
	// os.RemoveAll(absRoot)
	// os.Remove(absBinary)
}