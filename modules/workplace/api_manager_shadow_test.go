package workplace

import (
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/gin-gonic/gin"
)

type recordingWorkplaceShadowObserver struct {
	uid           string
	operationID   string
	legacyAllowed bool
	calls         int
}

func (r *recordingWorkplaceShadowObserver) Observe(uid, operationID string, legacyAllowed bool) {
	r.uid = uid
	r.operationID = operationID
	r.legacyAllowed = legacyAllowed
	r.calls++
}

func TestObserveWorkplaceShadowPreservesLegacyDecision(t *testing.T) {
	recorder := &recordingWorkplaceShadowObserver{}
	m := &manager{shadow: recorder}
	ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginContext.Set("uid", "u1")
	c := &wkhttp.Context{Context: ginContext}

	m.observeWorkplaceShadow(c, workplaceOperationAppList, errors.New("legacy denied"))
	if recorder.calls != 1 || recorder.uid != "u1" || recorder.operationID != workplaceOperationAppList || recorder.legacyAllowed {
		t.Fatalf("observer call = %#v, want denied legacy decision for u1/app list", recorder)
	}

	m.observeWorkplaceShadow(c, workplaceOperationAppList, nil)
	if recorder.calls != 2 || !recorder.legacyAllowed {
		t.Fatalf("observer call = %#v, want allowed legacy decision", recorder)
	}
}

func TestWorkplaceHandlersKeepDirectGatesAndDeclaredOperationIDs(t *testing.T) {
	sourceBytes, err := os.ReadFile("api_manager.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(sourceBytes)
	want := map[string]string{
		"addCategory":        "workplaceOperationCategoryCreate",
		"getCategory":        "workplaceOperationCategoryList",
		"reorderCategory":    "workplaceOperationCategoryReorder",
		"deleteCategory":     "workplaceOperationCategoryDelete",
		"updateCategory":     "workplaceOperationCategoryUpdate",
		"getCategoryApps":    "workplaceOperationCategoryAppList",
		"reorderCategoryApp": "workplaceOperationCategoryAppReorder",
		"addCategoryApp":     "workplaceOperationCategoryAppCreate",
		"deleteCategoryApp":  "workplaceOperationCategoryAppDelete",
		"addApp":             "workplaceOperationAppCreate",
		"getApps":            "workplaceOperationAppList",
		"updateApp":          "workplaceOperationAppUpdate",
		"deleteApp":          "workplaceOperationAppDelete",
		"addBanner":          "workplaceOperationBannerCreate",
		"getBanners":         "workplaceOperationBannerList",
		"deleteBanner":       "workplaceOperationBannerDelete",
		"updateBanner":       "workplaceOperationBannerUpdate",
		"reorderBanner":      "workplaceOperationBannerReorder",
	}
	for handler, operationID := range want {
		start := strings.Index(source, "func (m *manager) "+handler+"(")
		if start < 0 {
			t.Errorf("handler %s is missing", handler)
			continue
		}
		end := strings.Index(source[start+1:], "\nfunc (m *manager) ")
		if end < 0 {
			end = len(source) - start - 1
		}
		block := source[start : start+1+end]
		if !strings.Contains(block, "CheckLoginRole") {
			t.Errorf("handler %s lost its directly scannable legacy gate", handler)
		}
		if !strings.Contains(block, "m.observeWorkplaceShadow(c, "+operationID+", err)") {
			t.Errorf("handler %s is not mapped to %s", handler, operationID)
		}
	}
}
