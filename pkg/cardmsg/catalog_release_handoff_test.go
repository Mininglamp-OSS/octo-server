package cardmsg

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"testing"
)

const (
	catalogHandoffFixture       = "testdata/catalog-releases/docs.access-request-0.3.0.handoff.zip"
	catalogHandoffDigestFixture = "testdata/catalog-releases/docs.access-request-0.3.0.handoff.sha256"
)

type catalogHandoffManifest struct {
	ID                  string `json:"id"`
	Version             string `json:"version"`
	AdaptiveCardVersion string `json:"adaptiveCardVersion"`
	RenderProfile       string `json:"renderProfile"`
	Views               map[string]struct {
		WireProfile string   `json:"wireProfile"`
		Samples     []string `json:"samples"`
	} `json:"views"`
}

func TestCatalogReleaseHandoffPassesCardMessageGate(t *testing.T) {
	archiveBytes, err := os.ReadFile(catalogHandoffFixture)
	if err != nil {
		t.Fatal(err)
	}
	wantDigestBytes, err := os.ReadFile(catalogHandoffDigestFixture)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := strings.TrimSpace(string(wantDigestBytes))
	actualDigestBytes := sha256.Sum256(archiveBytes)
	actualDigest := hex.EncodeToString(actualDigestBytes[:])
	if actualDigest != wantDigest {
		t.Fatalf("Catalog Handoff SHA-256 = %s, want %s", actualDigest, wantDigest)
	}

	reader, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		t.Fatal(err)
	}
	files := make(map[string]*zip.File, len(reader.File))
	for _, file := range reader.File {
		files[file.Name] = file
	}

	const root = "docs.access-request@0.3.0"
	manifestBytes := readCatalogHandoffFile(t, files, root+"/manifest.json")
	var manifest catalogHandoffManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ID != "docs.access-request" || manifest.Version != "0.3.0" {
		t.Fatalf("Catalog Handoff identity = %s@%s", manifest.ID, manifest.Version)
	}
	if manifest.AdaptiveCardVersion != CardVersion {
		t.Fatalf("adaptiveCardVersion = %q, want %q", manifest.AdaptiveCardVersion, CardVersion)
	}
	if manifest.RenderProfile != "octo-chat@1.2.0-rc.4" {
		t.Fatalf("renderProfile = %q", manifest.RenderProfile)
	}

	validated := 0
	for view, spec := range manifest.Views {
		for _, sample := range spec.Samples {
			name := strings.TrimSuffix(path.Base(sample), path.Ext(sample))
			goldenPath := fmt.Sprintf("%s/goldens/%s.card.json", root, name)
			goldenBytes := readCatalogHandoffFile(t, files, goldenPath)
			var card map[string]interface{}
			if err := json.Unmarshal(goldenBytes, &card); err != nil {
				t.Fatalf("decode %s: %v", goldenPath, err)
			}
			originalCard, err := json.Marshal(card)
			if err != nil {
				t.Fatal(err)
			}
			payload := map[string]interface{}{
				"type":           InteractiveCard.Int(),
				"card":           card,
				"plain":          "untrusted producer text",
				"card_version":   manifest.AdaptiveCardVersion,
				"profile":        spec.WireProfile,
				"render_profile": RenderProfileOctoChatV1,
			}
			if err := Validate(payload); err != nil {
				t.Errorf("Validate(%s/%s): %v", view, name, err)
				continue
			}
			if err := Finalize(payload); err != nil {
				t.Errorf("Finalize(%s/%s): %v", view, name, err)
				continue
			}
			plain, _ := payload["plain"].(string)
			if strings.TrimSpace(plain) == "" || plain == "untrusted producer text" {
				t.Errorf("Finalize(%s/%s) did not derive authoritative plain text", view, name)
			}
			finalCard, err := json.Marshal(payload["card"])
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(finalCard, originalCard) {
				t.Errorf("Finalize(%s/%s) changed the Card document", view, name)
			}
			validated++
		}
	}
	if validated != 3 {
		t.Fatalf("validated golden count = %d, want 3", validated)
	}
}

func readCatalogHandoffFile(t *testing.T, files map[string]*zip.File, name string) []byte {
	t.Helper()
	file := files[name]
	if file == nil {
		t.Fatalf("Catalog Handoff is missing %s", name)
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
