package ui

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"sync"
)

type ConflictChoice int

const (
	KeepBoth ConflictChoice = iota
	UseCloud
	UseLocal
)

var (
	promptMu sync.Mutex
	scanner  = bufio.NewScanner(os.Stdin)
)

// ResolveConflict asks the user to resolve a conflict.
func ResolveConflict(loader *Loader, path, direction string) ConflictChoice {
	promptMu.Lock()
	defer promptMu.Unlock()

	// Pause spinner
	wasEnabled := false
	if loader != nil && loader.enabled {
		wasEnabled = true
		_ = loader.sp.Pause()
	}

	defer func() {
		// Resume spinner
		if loader != nil && wasEnabled {
			_ = loader.sp.Unpause()
		}
	}()

	// Clear line just in case
	fmt.Print("r")
	
	// Show prompt with explicit Println to safely handle newlines
	fmt.Println("")
	fmt.Printf("033[33m[CONFLICT]033[0m %s (%s)", path, direction)
	fmt.Println("") 
	fmt.Println("  [1] Keep Both (Rename new version)")
	fmt.Println("  [2] Use Cloud Version (Overwrite Local)")
	fmt.Println("  [3] Use Local Version (Overwrite Cloud)")
	fmt.Print("Select [1-3]: ")

	for {
		if !scanner.Scan() {
			return KeepBoth
		}
		text := strings.TrimSpace(scanner.Text())
		switch text {
		case "1", "":
			return KeepBoth
		case "2":
			return UseCloud
		case "3":
			return UseLocal
		default:
			fmt.Print("Invalid input. Select [1-3]: ")
		}
	}
}