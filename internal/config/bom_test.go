package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `{"client_id":"a","client_secret":"b","volume_step":7}`

// Notepad and PowerShell write a BOM by default. Editing the config the most
// obvious way on Windows must not leave a file the daemon cannot read.
func TestLoadAcceptsUTF8BOM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(sample)...)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("BOM should be tolerated: %v", err)
	}
	if c.VolumeStep != 7 {
		t.Fatalf("want step 7, got %d", c.VolumeStep)
	}
}

func TestLoadRejectsUTF16WithAClearMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte{0xFF, 0xFE, 0x7B, 0x00}, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "UTF-16") {
		t.Fatalf("error should name the real problem, got %q", err)
	}
}

// Absent keys keep their defaults; present keys win even when zero-valued,
// which is what makes "hotkeys": false work.
func TestLoadMergesOverDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"client_id":"a","client_secret":"b","hotkeys":false,"osd":{"scale":1.5}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Hotkeys {
		t.Fatal("hotkeys:false must survive the merge")
	}
	if c.VolumeStep != Default().VolumeStep {
		t.Fatalf("absent key should keep its default, got %d", c.VolumeStep)
	}
	if c.OSD.Scale != 1.5 {
		t.Fatalf("want scale 1.5, got %v", c.OSD.Scale)
	}
	if !c.OSD.Enabled {
		t.Fatal("absent nested key should keep its default")
	}
}
