package _go

import (
	"fmt"
	"github.com/fsnotify/fsnotify"
	"log"
	"os"
	"os/exec"
	"path/filepath"
)

// isGoImportsAvailable checks if the `goimports` command is available
func isGoImportsAvailable() bool {
	_, err := exec.LookPath("goimports")
	return err == nil
}

// installGoImports installs the `goimports` tool using `go install`
func installGoImports() error {
	cmd := exec.Command("go", "install", "golang.org/x/tools/cmd/goimports@latest")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildProject(appPath, outPath string) error {
	absAppPath, err := filepath.Abs(appPath)
	if err != nil {
		fmt.Println("failed to resolve absolute app path:", err)
		return err
	}

	// Step 1: go mod download
	cmdDownload := exec.Command("go", "mod", "download")
	cmdDownload.Dir = absAppPath
	cmdDownload.Stdout = os.Stdout
	cmdDownload.Stderr = os.Stderr
	if err := cmdDownload.Run(); err != nil {
		fmt.Println("failed to download modules:", err)
		return err
	}

	// Step 2: GOOS=linux GOARCH=amd64 go build -o "$OUT_PATH" .
	cmdBuild := exec.Command("go", "build", "-o", outPath, ".")
	cmdBuild.Dir = absAppPath
	//cmdBuild.Env = append(os.Environ(),
	//	"GOOS=linux",
	//	"GOARCH=amd64",
	//)
	cmdBuild.Stdout = os.Stdout
	cmdBuild.Stderr = os.Stderr
	if err := cmdBuild.Run(); err != nil {
		fmt.Println("failed to build binary:", err)
		return err
	}

	fmt.Println("build completed successfully:", outPath)
	return nil
}

type Generator struct {
}

func (g *Generator) Init() error {
	// Check if `goimports` is installed
	if !isGoImportsAvailable() {
		log.Println("goimports is not installed. Installing now...")

		err := installGoImports()
		if err != nil {
			log.Printf("failed to install goimports: %s\nplease install it manually by running:\n\tgo install golang.org/x/tools/cmd/goimports@latest\n", err.Error())
			return err
		}

		log.Println("goimports successfully installed.")
	}

	return nil
}

func (g *Generator) OnChange(appPath string, outputPath string, event fsnotify.Event) error {
	if IsGoFile(event.Name) {
		if err := CheckFileCompilable(event.Name); err == nil {
			log.Printf("change detected in: %s, triggering generate\n", event.Name)
			return g.Generate(appPath, outputPath)
		} else {
			log.Printf("file not compilable: %s, error: %s\n", event.Name, err.Error())
			return err
		}
	}

	return nil
}

func (g *Generator) Generate(appPath string, outputPath string) error {
	err := generateServices(appPath, true)
	if err != nil {
		fmt.Printf("error generating services, error: %s\n", err.Error())
		return err
	}

	err = generateRuntime(appPath)
	if err != nil {
		fmt.Printf("error generating runtime, error: %s\n", err.Error())
		return err
	}

	err = buildProject(appPath, outputPath)
	if err != nil {
		fmt.Printf("error building project, error: %s\n", err.Error())
		return err
	}

	return nil
}
