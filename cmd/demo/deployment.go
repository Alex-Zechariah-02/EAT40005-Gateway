package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const identityFile = "demo.json"

// demoIdentity binds state-changing demonstration operations to the exact
// directory and files setup created. It grants no policy or admin authority.
type demoIdentity struct {
	Format    string `json:"format"`
	Directory string `json:"directory"`
	Config    string `json:"config_sha256"`
	Secret    string `json:"fixture_secret_sha256"`
}

func demoDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func identifyDemo(dir string) error {
	config, err := demoDigest(filepath.Join(dir, configFile))
	if err != nil {
		return err
	}
	secret, err := demoDigest(filepath.Join(dir, secretFile))
	if err != nil {
		return err
	}
	identity := demoIdentity{Format: "gateway-demo-1", Directory: dir, Config: config, Secret: secret}
	return os.WriteFile(filepath.Join(dir, identityFile), indented(identity), 0o600)
}

func checkDemo(dir string) error {
	for _, name := range []string{identityFile, configFile, privateKeyFile, publicKeyFile, secretFile, databaseFile, "gateway.log", "orders.log", "inventory.log", "reports.log"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("demonstration file %s must be a regular file, not a link or directory", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "caddy.json")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("legacy Caddy demonstration configuration is unsupported; create a new native demonstration directory")
	}
	file, err := os.Open(filepath.Join(dir, identityFile))
	if err != nil {
		return fmt.Errorf("not an identified demonstration directory: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	decoder.DisallowUnknownFields()
	var identity demoIdentity
	if err := decoder.Decode(&identity); err != nil {
		return fmt.Errorf("invalid demonstration identity: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("data after the demonstration identity")
	}
	config, err := demoDigest(filepath.Join(dir, configFile))
	if err != nil {
		return err
	}
	secret, err := demoDigest(filepath.Join(dir, secretFile))
	if err != nil {
		return err
	}
	if identity.Format != "gateway-demo-1" || identity.Directory != dir || identity.Config != config || identity.Secret != secret {
		return errors.New("demonstration directory or configuration no longer matches setup; refusing the operation")
	}
	return nil
}

// readAdmission reads the native controller's startup observation. Listening
// alone is not readiness, and bootstrap/recovery are reported as such.
func readAdmission(path string, start int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event struct {
			Message   string `json:"msg"`
			Admission string `json:"admission"`
		}
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Message == "protected admission" {
			return event.Admission, nil
		}
	}
	return "", scanner.Err()
}
