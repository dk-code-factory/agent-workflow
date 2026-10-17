package process

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type Completion struct {
	Token   string `json:"token"`
	Cleaned bool   `json:"cleaned"`
	Error   string `json:"error,omitempty"`
}
type SupervisionOptions struct {
	Cleanup        []string
	CompletionPath string
	Token          string
}

func writeCompletion(path, token string, cleanupErr error) error {
	if path == "" {
		return nil
	}
	value := Completion{Token: token, Cleaned: cleanupErr == nil}
	if cleanupErr != nil {
		value.Error = "supervised cleanup was not confirmed"
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path+".partial", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path+".partial", path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ReadCompletion(path, token string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("invalid guardian completion record")
	}
	var value Completion
	if json.Unmarshal(data, &value) != nil || value.Token != token || !value.Cleaned {
		return fmt.Errorf("%w: invalid or uncertain guardian completion", ErrCleanup)
	}
	return nil
}
