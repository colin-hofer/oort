package manifest

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	FileName       = "nebulous.json"
	MaxBundleBytes = 25 << 20
	MaxBundleFiles = 1000
)

var (
	slugPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	parameterPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

type Manifest struct {
	App     App     `json:"app"`
	Queries []Query `json:"queries"`
}

type App struct {
	Slug string `json:"slug"`
	Dir  string `json:"dir"`
}

type Query struct {
	Name       string            `json:"name"`
	File       string            `json:"file"`
	Parameters map[string]string `json:"parameters"`
}

func Load(file string) (Manifest, error) {
	contents, err := os.ReadFile(file)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", FileName, err)
	}
	return Parse(contents)
}

func Parse(contents []byte) (Manifest, error) {
	var value Manifest
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", FileName, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, fmt.Errorf("%s must contain one JSON object", FileName)
	}
	if err := value.Validate(); err != nil {
		return Manifest{}, err
	}
	return value, nil
}

func (m Manifest) Validate() error {
	if !slugPattern.MatchString(m.App.Slug) || strings.Contains(m.App.Slug, "--") {
		return fmt.Errorf("app slug must be 3-63 lowercase letters, digits, or hyphens")
	}
	if !safePath(m.App.Dir) {
		return fmt.Errorf("app dir must be a relative directory inside the project")
	}
	names := make(map[string]bool, len(m.Queries))
	files := make(map[string]bool, len(m.Queries))
	for _, query := range m.Queries {
		if !slugPattern.MatchString(query.Name) {
			return fmt.Errorf("query name %q must be 3-63 lowercase letters, digits, or hyphens", query.Name)
		}
		if names[query.Name] {
			return fmt.Errorf("query name %q is duplicated", query.Name)
		}
		names[query.Name] = true
		if !safePath(query.File) || strings.ToLower(path.Ext(query.File)) != ".sql" {
			return fmt.Errorf("query %q file must be a relative .sql file inside the project", query.Name)
		}
		if files[query.File] {
			return fmt.Errorf("query file %q is used more than once", query.File)
		}
		files[query.File] = true
		for name, kind := range query.Parameters {
			if !parameterPattern.MatchString(name) {
				return fmt.Errorf("query %q has invalid parameter name %q", query.Name, name)
			}
			switch kind {
			case "boolean", "integer", "number", "string":
			default:
				return fmt.Errorf("query %q parameter %q has unsupported type %q", query.Name, name, kind)
			}
		}
	}
	return nil
}

func BuildBundle(projectDir string, m Manifest, output io.Writer) error {
	if err := m.Validate(); err != nil {
		return err
	}
	files := map[string]string{}
	appRoot := filepath.Join(projectDir, filepath.FromSlash(m.App.Dir))
	if err := filepath.WalkDir(appRoot, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("bundle cannot contain symlink %s", file)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("bundle can contain only regular files: %s", file)
		}
		relative, err := filepath.Rel(projectDir, file)
		if err != nil || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("asset is outside the project: %s", file)
		}
		files[filepath.ToSlash(relative)] = file
		return nil
	}); err != nil {
		return fmt.Errorf("read app dir: %w", err)
	}
	for _, query := range m.Queries {
		file := filepath.Join(projectDir, filepath.FromSlash(query.File))
		info, err := os.Lstat(file)
		if err != nil {
			return fmt.Errorf("read query %q: %w", query.Name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("query %q must be a regular file", query.Name)
		}
		files[query.File] = file
	}
	if len(files)+1 > MaxBundleFiles {
		return fmt.Errorf("bundle exceeds %d files", MaxBundleFiles)
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	archive := zip.NewWriter(output)
	manifestJSON, _ := json.Marshal(m)
	if err := writeZipFile(archive, FileName, manifestJSON); err != nil {
		return err
	}
	var total int64 = int64(len(manifestJSON))
	for _, name := range names {
		contents, err := os.ReadFile(files[name])
		if err != nil {
			return fmt.Errorf("read bundle file %s: %w", name, err)
		}
		total += int64(len(contents))
		if total > MaxBundleBytes {
			return fmt.Errorf("bundle contents exceed %d bytes", MaxBundleBytes)
		}
		if err := writeZipFile(archive, name, contents); err != nil {
			return err
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finish app bundle: %w", err)
	}
	return nil
}

func ReadBundle(contents []byte) (Manifest, map[string]string, error) {
	files, err := readFiles(contents)
	if err != nil {
		return Manifest{}, nil, err
	}
	manifestJSON, ok := files[FileName]
	if !ok {
		return Manifest{}, nil, fmt.Errorf("bundle is missing %s", FileName)
	}
	m, err := Parse(manifestJSON)
	if err != nil {
		return Manifest{}, nil, err
	}
	queries := make(map[string]string, len(m.Queries))
	allowed := map[string]bool{FileName: true}
	for name := range files {
		if strings.HasPrefix(name, m.App.Dir+"/") {
			allowed[name] = true
		}
	}
	if _, ok := files[m.App.Dir+"/index.html"]; !ok {
		return Manifest{}, nil, fmt.Errorf("app dir must contain index.html")
	}
	for _, query := range m.Queries {
		contents, ok := files[query.File]
		if !ok {
			return Manifest{}, nil, fmt.Errorf("bundle is missing query file %s", query.File)
		}
		allowed[query.File] = true
		queries[query.Name] = string(contents)
	}
	for name := range files {
		if !allowed[name] {
			return Manifest{}, nil, fmt.Errorf("bundle contains undeclared file %s", name)
		}
	}
	return m, queries, nil
}

func Asset(contents []byte, appDir, requestPath string) ([]byte, string, error) {
	requestPath = strings.TrimPrefix(requestPath, "/")
	if requestPath == "" {
		requestPath = "index.html"
	}
	if !safePath(requestPath) {
		return nil, "", fs.ErrNotExist
	}
	name := path.Join(appDir, requestPath)
	reader, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
	if err != nil {
		return nil, "", fmt.Errorf("open app bundle: %w", err)
	}
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		opened, err := file.Open()
		if err != nil {
			return nil, "", err
		}
		defer opened.Close()
		asset, err := io.ReadAll(io.LimitReader(opened, MaxBundleBytes+1))
		if err != nil || len(asset) > MaxBundleBytes {
			return nil, "", fmt.Errorf("read app asset")
		}
		return asset, path.Ext(name), nil
	}
	return nil, "", fs.ErrNotExist
}

func readFiles(contents []byte) (map[string][]byte, error) {
	if len(contents) > MaxBundleBytes {
		return nil, fmt.Errorf("bundle exceeds %d bytes", MaxBundleBytes)
	}
	reader, err := zip.NewReader(bytes.NewReader(contents), int64(len(contents)))
	if err != nil {
		return nil, fmt.Errorf("open app bundle: %w", err)
	}
	if len(reader.File) > MaxBundleFiles {
		return nil, fmt.Errorf("bundle exceeds %d files", MaxBundleFiles)
	}
	files := make(map[string][]byte, len(reader.File))
	var total int64
	for _, file := range reader.File {
		if !safePath(file.Name) || file.FileInfo().IsDir() || file.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("bundle contains unsafe file %q", file.Name)
		}
		if _, exists := files[file.Name]; exists {
			return nil, fmt.Errorf("bundle contains duplicate file %q", file.Name)
		}
		if file.UncompressedSize64 > MaxBundleBytes || total > MaxBundleBytes-int64(file.UncompressedSize64) {
			return nil, fmt.Errorf("bundle contents exceed %d bytes", MaxBundleBytes)
		}
		total += int64(file.UncompressedSize64)
		opened, err := file.Open()
		if err != nil {
			return nil, err
		}
		value, readErr := io.ReadAll(io.LimitReader(opened, MaxBundleBytes+1))
		closeErr := opened.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		files[file.Name] = value
	}
	return files, nil
}

func writeZipFile(archive *zip.Writer, name string, contents []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o644)
	file, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create bundle file %s: %w", name, err)
	}
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write bundle file %s: %w", name, err)
	}
	return nil
}

func safePath(value string) bool {
	return value != "" && value != "." && !strings.HasPrefix(value, "/") &&
		!strings.Contains(value, `\`) && path.Clean(value) == value && !strings.HasPrefix(value, "../")
}
