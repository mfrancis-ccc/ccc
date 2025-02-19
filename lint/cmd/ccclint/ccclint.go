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
	// Read the .golangci.yml file
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

	// Get the custom linter path and pull out the version
	parts := strings.Split(config.Linters.Custom.CCCLint.Path, string(filepath.Separator))

	if len(parts) < 2 {
		fmt.Printf("Error: invalid path %s\n", config.Linters.Custom.CCCLint.Path)
		os.Exit(1)
	}

	version := parts[len(parts)-2]

	// TODO: Check if file already exists
	fi, err := os.Stat(fmt.Sprintf("tmp/ccc-lint/%s/ccc-lint.so", version))
	if err == nil && fi != nil {
		fmt.Println("ccc-lint already exists")
		return
	}

	// Running the go get command just makes sure the .so file will exist in the cache
	execCommand("go", "get", fmt.Sprintf("github.com/cccteam/ccc/lint@%s", version))

	if err := os.MkdirAll(fmt.Sprintf("tmp/ccc-lint/%s", version), 0o744); err != nil {
		fmt.Printf("Error: %+v\n", err)
		os.Exit(1)
	}

	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		gopath = build.Default.GOPATH
	}

	// Pull the .so file from the go cache
	execCommand("cp", fmt.Sprintf("%s/pkg/mod/github.com/cccteam/ccc/lint@%s/ccc-lint.so", gopath, version), fmt.Sprintf("tmp/ccc-lint/%s/ccc-lint.so", version))

	// Tidy is just here to make sure the go.mod file is cleaned up afterwards
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
