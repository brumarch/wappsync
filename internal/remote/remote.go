// Package remote publica e lê arquivos no destino em nuvem.
//
// Dois backends:
//   - folder: escreve numa pasta local que o cliente do Google Drive/OneDrive/
//     Dropbox sincroniza. Zero credenciais no script.
//   - rclone: chama o binário rclone (Drive via API, S3, B2, R2, ...).
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brumarch/wappsync/internal/config"
)

// ErrNotExist é devolvido por Get quando o arquivo não existe no destino.
var ErrNotExist = os.ErrNotExist

// ErrUnsafePath recusa um caminho que sairia da raiz do destino.
//
// Delete é a única operação destrutiva desta interface, e os caminhos que
// chegam nela derivam de arquivos lidos da pasta compartilhada. Um ".." ali
// apagaria o consolidado em vez de um anexo.
var ErrUnsafePath = errors.New("caminho relativo inseguro")

// safeRelPath recusa caminho absoluto, vazio ou com "..".
func safeRelPath(relPath string) error {
	if relPath == "" || strings.HasPrefix(relPath, "/") || strings.Contains(relPath, "\\") {
		return fmt.Errorf("%w: %q", ErrUnsafePath, relPath)
	}
	for _, part := range strings.Split(relPath, "/") {
		if part == ".." {
			return fmt.Errorf("%w: %q", ErrUnsafePath, relPath)
		}
	}
	return nil
}

type Backend interface {
	// Put grava data em relPath (caminho relativo com "/"), atomicamente quando possível.
	Put(ctx context.Context, relPath string, data []byte) error
	// Get lê relPath. Devolve ErrNotExist se não houver.
	Get(ctx context.Context, relPath string) ([]byte, error)
	// List lista os arquivos diretamente sob relDir (nomes, não caminhos).
	List(ctx context.Context, relDir string) ([]string, error)
	// Delete remove relPath. Apagar o que não existe não é erro.
	Delete(ctx context.Context, relPath string) error
	// Describe é uma descrição legível do destino, para logs.
	Describe() string
}

// New devolve o backend configurado. Para backend "none" devolve nil.
func New(cfg *config.Config) (Backend, error) {
	switch cfg.Remote.Backend {
	case "folder":
		return &folder{root: filepath.Join(cfg.Remote.Folder.Path, cfg.Remote.Prefix)}, nil
	case "rclone":
		bin, err := exec.LookPath(cfg.Remote.Rclone.Binary)
		if err != nil {
			return nil, fmt.Errorf("rclone não encontrado no PATH (%q): %w", cfg.Remote.Rclone.Binary, err)
		}
		root := strings.TrimSuffix(cfg.Remote.Rclone.Remote, "/")
		if cfg.Remote.Prefix != "" {
			root += "/" + cfg.Remote.Prefix
		}
		return &rclone{bin: bin, root: root}, nil
	case "none":
		return nil, nil
	}
	return nil, fmt.Errorf("backend desconhecido: %s", cfg.Remote.Backend)
}

// ---------------------------------------------------------------- folder ---

type folder struct{ root string }

func (f *folder) Describe() string { return "pasta " + f.root }

func (f *folder) abs(relPath string) string {
	return filepath.Join(f.root, filepath.FromSlash(relPath))
}

func (f *folder) Put(_ context.Context, relPath string, data []byte) error {
	dst := f.abs(relPath)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// Escrita atômica: os clientes de sincronização não sobem arquivos parciais.
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// No Windows os.Rename falha se o destino existe.
	if err := os.Rename(tmpName, dst); err != nil {
		if rmErr := os.Remove(dst); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(tmpName, dst); err != nil {
			return err
		}
	}
	return nil
}

func (f *folder) Delete(_ context.Context, relPath string) error {
	if err := safeRelPath(relPath); err != nil {
		return err
	}
	if err := os.Remove(f.abs(relPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (f *folder) Get(_ context.Context, relPath string) ([]byte, error) {
	data, err := os.ReadFile(f.abs(relPath))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotExist
	}
	return data, err
}

func (f *folder) List(_ context.Context, relDir string) ([]string, error) {
	entries, err := os.ReadDir(f.abs(relDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out, nil
}

// ---------------------------------------------------------------- rclone ---

type rclone struct {
	bin  string
	root string
}

func (r *rclone) Describe() string { return "rclone " + r.root }

func (r *rclone) remotePath(relPath string) string {
	return r.root + "/" + path.Clean(relPath)
}

func (r *rclone) run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.bin, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, "directory not found") || strings.Contains(msg, "object not found") {
			return nil, ErrNotExist
		}
		return nil, fmt.Errorf("rclone %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return stdout.Bytes(), nil
}

func (r *rclone) Put(ctx context.Context, relPath string, data []byte) error {
	_, err := r.run(ctx, data, "rcat", r.remotePath(relPath))
	return err
}

func (r *rclone) Get(ctx context.Context, relPath string) ([]byte, error) {
	return r.run(ctx, nil, "cat", r.remotePath(relPath))
}

func (r *rclone) Delete(ctx context.Context, relPath string) error {
	if err := safeRelPath(relPath); err != nil {
		return err
	}
	_, err := r.run(ctx, nil, "deletefile", r.remotePath(relPath))
	if errors.Is(err, ErrNotExist) {
		return nil
	}
	return err
}

func (r *rclone) List(ctx context.Context, relDir string) ([]string, error) {
	out, err := r.run(ctx, nil, "lsf", "--files-only", r.remotePath(relDir))
	if errors.Is(err, ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
