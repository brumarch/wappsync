package config

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed example.toml
var exampleTOML string

// Example devolve o conteúdo do arquivo de configuração de exemplo.
func Example() string { return exampleTOML }

// WriteExample cria path com a configuração de exemplo. Não sobrescreve.
func WriteExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s já existe", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(exampleTOML), 0o600)
}
