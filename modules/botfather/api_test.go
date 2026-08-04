package botfather

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	pkgutil "github.com/Mininglamp-OSS/octo-server/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
)

func TestObjectPathFormat(t *testing.T) {
	filename := "qualcomm_review.xlsx"
	objectPath := fmt.Sprintf("chat/%d/%s/%s", time.Now().Unix(), util.GenerUUID(), url.PathEscape(filename))
	parts := strings.Split(objectPath, "/")
	if len(parts) != 4 {
		t.Fatalf("expected 4 path segments, got %d: %s", len(parts), objectPath)
	}
	if parts[len(parts)-1] != filename {
		t.Errorf("expected last segment to be %q, got %q", filename, parts[len(parts)-1])
	}
}

func TestObjectPathFormatWithSpecialChars(t *testing.T) {
	filename := "报告 2024.xlsx"
	objectPath := fmt.Sprintf("chat/%d/%s/%s", time.Now().Unix(), util.GenerUUID(), url.PathEscape(filename))
	parts := strings.Split(objectPath, "/")
	lastPart := parts[len(parts)-1]
	decoded, err := url.PathUnescape(lastPart)
	if err != nil {
		t.Fatalf("failed to unescape last segment: %v", err)
	}
	if decoded != filename {
		t.Errorf("expected decoded last segment to be %q, got %q", filename, decoded)
	}
}

func TestLegacyUUIDStripping(t *testing.T) {
	path := "chat/1713360000/afd1a8d99bb94bf0a8d2c1e3f4a5b6c7_report.xlsx"
	got := pkgutil.ExtractFilenameFromPath(path)
	if got != "report.xlsx" {
		t.Errorf("expected %q, got %q", "report.xlsx", got)
	}
}

func TestLegacyUUIDStrippingWithEncoding(t *testing.T) {
	path := "chat/1713360000/afd1a8d99bb94bf0a8d2c1e3f4a5b6c7_" + url.PathEscape("报告.xlsx")
	got := pkgutil.ExtractFilenameFromPath(path)
	if got != "报告.xlsx" {
		t.Errorf("expected %q, got %q", "报告.xlsx", got)
	}
}

func TestLegacyNoFalsePositive(t *testing.T) {
	// Filename that happens to have _ at various positions but no valid 32-char hex prefix
	path := "chat/1713360000/my_very_long_filename_with_underscores.xlsx"
	got := pkgutil.ExtractFilenameFromPath(path)
	if got != "my_very_long_filename_with_underscores.xlsx" {
		t.Errorf("expected %q, got %q", "my_very_long_filename_with_underscores.xlsx", got)
	}
}

func TestNewPathLastSegment(t *testing.T) {
	uuid := util.GenerUUID()
	path := fmt.Sprintf("chat/%d/%s/file.xlsx", time.Now().Unix(), uuid)
	got := pkgutil.ExtractFilenameFromPath(path)
	if got != "file.xlsx" {
		t.Errorf("expected %q, got %q", "file.xlsx", got)
	}
}

func TestNewPathEncodedFilename(t *testing.T) {
	uuid := util.GenerUUID()
	filename := "my report (final).xlsx"
	path := fmt.Sprintf("chat/%d/%s/%s", time.Now().Unix(), uuid, url.PathEscape(filename))
	got := pkgutil.ExtractFilenameFromPath(path)
	if got != filename {
		t.Errorf("expected %q, got %q", filename, got)
	}
}
