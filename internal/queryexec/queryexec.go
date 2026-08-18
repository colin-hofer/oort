package queryexec

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"nebulous/internal/storage"
)

const maxRequestBytes = 2 << 20

var (
	errOutputLimit = errors.New("query result exceeded its byte limit")
	parameterName  = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	dollarQuote    = regexp.MustCompile(`\$[A-Za-z_][A-Za-z0-9_]*\$|\$\$`)
	wordPattern    = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	forbiddenWord  = map[string]bool{
		"alter": true, "attach": true, "call": true, "checkpoint": true,
		"copy": true, "create": true, "delete": true, "detach": true,
		"drop": true, "export": true, "force": true, "import": true,
		"insert": true, "install": true, "load": true, "merge": true,
		"pragma": true, "reset": true, "set": true, "update": true,
		"vacuum": true,
	}
)

type Request struct {
	CatalogURL     string            `json:"catalog_url"`
	DataPath       string            `json:"data_path"`
	ExtensionDir   string            `json:"extension_dir"`
	TempDir        string            `json:"temp_dir"`
	Storage        storage.Config    `json:"storage"`
	SQL            string            `json:"sql"`
	Parameters     map[string]any    `json:"parameters"`
	ParameterTypes map[string]string `json:"parameter_types"`
	MaxRows        int               `json:"max_rows"`
	MaxBytes       int64             `json:"max_bytes"`
}

type Column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func Run(ctx context.Context, executable string, request Request, result io.Writer) error {
	tempDir, err := os.MkdirTemp("", "nebulous-query-*")
	if err != nil {
		return fmt.Errorf("create query temporary directory: %w", err)
	}
	defer os.RemoveAll(tempDir)
	request.TempDir = tempDir
	framed, err := EncodeRequest(request)
	if err != nil {
		return err
	}
	command := exec.Command(executable, "__query-exec")
	command.Stdin = bytes.NewReader(framed)
	command.Env = scrubEnvironment(os.Environ())
	setProcessGroup(command)
	output := &limitWriter{writer: result, remaining: request.MaxBytes}
	var stderr bytes.Buffer
	command.Stdout = output
	command.Stderr = &limitWriter{writer: &stderr, remaining: 64 << 10}
	if err := command.Start(); err != nil {
		return fmt.Errorf("start query process: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if errors.Is(output.err, errOutputLimit) {
			return errOutputLimit
		}
		if err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return fmt.Errorf("query failed: %s", message)
		}
		return nil
	case <-ctx.Done():
		terminateProcessGroup(command.Process.Pid)
		select {
		case <-done:
		case <-time.After(250 * time.Millisecond):
			killProcessGroup(command.Process.Pid)
			<-done
		}
		return ctx.Err()
	}
}

func EncodeRequest(request Request) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode query request: %w", err)
	}
	if len(payload) > maxRequestBytes {
		return nil, fmt.Errorf("query request exceeds %d bytes", maxRequestBytes)
	}
	framed := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(framed, uint32(len(payload)))
	copy(framed[4:], payload)
	return framed, nil
}

func DecodeRequest(input io.Reader) (Request, error) {
	var header [4]byte
	if _, err := io.ReadFull(input, header[:]); err != nil {
		return Request{}, fmt.Errorf("read query request header: %w", err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > maxRequestBytes {
		return Request{}, fmt.Errorf("invalid query request size %d", size)
	}
	decoder := json.NewDecoder(io.LimitReader(input, int64(size)))
	decoder.UseNumber()
	var request Request
	if err := decoder.Decode(&request); err != nil {
		return Request{}, fmt.Errorf("decode query request: %w", err)
	}
	return request, nil
}

func Validate(sqlText string, parameters map[string]any) (string, map[string]string, error) {
	cleaned, err := readableSQL(sqlText)
	if err != nil {
		return "", nil, err
	}
	words := wordPattern.FindAllString(cleaned, -1)
	if len(words) == 0 || (strings.ToLower(words[0]) != "select" && strings.ToLower(words[0]) != "with") {
		return "", nil, fmt.Errorf("query must be a SELECT or WITH statement")
	}
	for _, word := range words {
		if forbiddenWord[strings.ToLower(word)] {
			return "", nil, fmt.Errorf("query contains forbidden statement %s", strings.ToUpper(word))
		}
	}
	referenced := map[string]bool{}
	for _, match := range parameterName.FindAllStringSubmatch(cleaned, -1) {
		referenced[match[1]] = true
	}
	for name := range referenced {
		if _, ok := parameters[name]; !ok {
			return "", nil, fmt.Errorf("missing query parameter %q", name)
		}
	}
	for name := range parameters {
		if !referenced[name] {
			return "", nil, fmt.Errorf("unknown query parameter %q", name)
		}
	}
	types := make(map[string]string, len(parameters))
	for name, value := range parameters {
		typeName, err := parameterType(value)
		if err != nil {
			return "", nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		types[name] = typeName
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sqlText), ";")), types, nil
}

func readableSQL(sqlText string) (string, error) {
	text := strings.TrimSpace(sqlText)
	if strings.HasSuffix(text, ";") {
		text = strings.TrimSpace(strings.TrimSuffix(text, ";"))
	}
	if text == "" || strings.ContainsRune(text, 0) {
		return "", fmt.Errorf("query is empty or contains NUL")
	}
	if dollarQuote.MatchString(text) {
		return "", fmt.Errorf("dollar-quoted SQL is not allowed")
	}
	result := []byte(text)
	for index := 0; index < len(result); {
		switch {
		case result[index] == ';':
			return "", fmt.Errorf("query must contain exactly one statement")
		case result[index] == '\'' || result[index] == '"':
			quote := result[index]
			result[index] = ' '
			index++
			closed := false
			for index < len(result) {
				if result[index] == quote {
					result[index] = ' '
					if index+1 < len(result) && result[index+1] == quote {
						result[index+1] = ' '
						index += 2
						continue
					}
					index++
					closed = true
					break
				}
				result[index] = ' '
				index++
			}
			if !closed {
				return "", fmt.Errorf("query contains an unterminated quoted value")
			}
		case index+1 < len(result) && result[index] == '-' && result[index+1] == '-':
			for index < len(result) && result[index] != '\n' {
				result[index] = ' '
				index++
			}
		case index+1 < len(result) && result[index] == '/' && result[index+1] == '*':
			result[index], result[index+1] = ' ', ' '
			index += 2
			closed := false
			for index+1 < len(result) {
				if result[index] == '*' && result[index+1] == '/' {
					result[index], result[index+1] = ' ', ' '
					index += 2
					closed = true
					break
				}
				result[index] = ' '
				index++
			}
			if !closed {
				return "", fmt.Errorf("query contains an unterminated comment")
			}
		default:
			index++
		}
	}
	return string(result), nil
}

func parameterType(value any) (string, error) {
	switch number := value.(type) {
	case string:
		return "string", nil
	case bool:
		return "boolean", nil
	case float64:
		if number == float64(int64(number)) {
			return "integer", nil
		}
		return "number", nil
	case json.Number:
		if strings.ContainsAny(number.String(), ".eE") {
			if _, err := number.Float64(); err != nil {
				return "", fmt.Errorf("invalid number")
			}
			return "number", nil
		}
		if _, err := number.Int64(); err != nil {
			return "", fmt.Errorf("integer is out of range")
		}
		return "integer", nil
	case nil:
		return "", fmt.Errorf("null values need an explicit type")
	default:
		return "", fmt.Errorf("must be a string, number, or boolean")
	}
}

func normalizedParameters(parameters map[string]any) []any {
	names := make([]string, 0, len(parameters))
	for name := range parameters {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]any, 0, len(names))
	for _, name := range names {
		value := parameters[name]
		if number, ok := value.(json.Number); ok {
			if integer, err := number.Int64(); err == nil {
				value = integer
			} else if decimal, err := number.Float64(); err == nil {
				value = decimal
			}
		}
		values = append(values, sql.Named(name, value))
	}
	return values
}

func scrubEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		name, _, _ := strings.Cut(item, "=")
		upper := strings.ToUpper(name)
		if strings.HasPrefix(upper, "AWS_") || strings.HasPrefix(upper, "AZURE_") ||
			strings.HasPrefix(upper, "GOOGLE_") || strings.HasPrefix(upper, "GCP_") ||
			strings.HasPrefix(upper, "MINIO_") || strings.HasPrefix(upper, "S3_") ||
			strings.HasPrefix(upper, "DUCKDB_") || strings.HasPrefix(upper, "NEB_") ||
			strings.HasPrefix(upper, "PG") || strings.HasSuffix(upper, "_PROXY") {
			continue
		}
		result = append(result, item)
	}
	return result
}

type limitWriter struct {
	writer    io.Writer
	remaining int64
	err       error
}

func (w *limitWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(data)) > w.remaining {
		w.err = errOutputLimit
		return 0, w.err
	}
	count, err := w.writer.Write(data)
	w.remaining -= int64(count)
	if err != nil {
		w.err = err
	}
	return count, err
}
