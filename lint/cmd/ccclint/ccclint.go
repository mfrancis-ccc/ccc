package ccclint

import (
	"fmt"
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

	temp := filepath.Base(config.Linters.Custom.CCCLint.Path)
	temp = strings.TrimSuffix(temp, filepath.Ext(temp))

	parts := strings.Split(temp, "@")
	if len(parts) != 2 {
		fmt.Printf("Error: invalid path %s\n", config.Linters.Custom.CCCLint.Path)
		os.Exit(1)
	}

	version := parts[1]

	execCommand("go", "get", fmt.Sprintf("github.com/cccteam/ccc/lint@%s", version))

	execCommand("cp", fmt.Sprintf("$(go env GOPATH)/pkg/mod/github.com/cccteam/ccc/lint@%s/ccc-lint.so", version), fmt.Sprintf("~/.local/ccc-lint/%s/ccc-lint.so", version))

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
