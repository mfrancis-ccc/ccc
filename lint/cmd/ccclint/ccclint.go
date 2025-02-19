package main

import (
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Linters LintersSettings `yaml:"linters-settings"`
}

type LintersSettings struct {
	Custom Custom `yaml:"custom"`
}

type Custom struct {
	CCCLint Linter `yaml:"ccclint"`
}

type Linter struct {
	Path string `yaml:"path"`
}

func main() {
	file, err := os.Open(".golangci.yml")
	if err != nil {
		fmt.Printf("Error: %+v\n", err)
		os.Exit(1)
	}

	var config Config
	if err := yaml.NewDecoder(file).Decode(&config); err != nil {
		fmt.Printf("Error: %+v\n", err)
		os.Exit(1)
	}

	parts := strings.Split(config.Linters.Custom.CCCLint.Path, string(filepath.Separator))

	if len(parts) < 2 {
		fmt.Printf("Error: invalid path %s\n", config.Linters.Custom.CCCLint.Path)
		os.Exit(1)
	}

	version := parts[len(parts)-2]

	fmt.Println(version)

	execCommand("go", "get", fmt.Sprintf("github.com/cccteam/ccc/lint@%s", version))

	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}
	fmt.Println(gopath)

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Error: %+v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(fmt.Sprintf("%s/.local/ccc-lint/%s", homeDir, version), 0o744); err != nil {
		fmt.Printf("Error: %+v\n", err)
		os.Exit(1)
	}

	execCommand("cp", fmt.Sprintf("%s/pkg/mod/github.com/cccteam/ccc/lint@%s/ccc-lint.so", gopath, version), fmt.Sprintf("%s/.local/ccc-lint/%s/ccc-lint.so", homeDir, version))

	execCommand("go", "mod", "tidy")
}

func execCommand(command string, args ...string) {
	cmd := exec.Command(command, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Printf("Error: %+v\n", err)
		fmt.Println(string(out))
		os.Exit(1)
	}
}
