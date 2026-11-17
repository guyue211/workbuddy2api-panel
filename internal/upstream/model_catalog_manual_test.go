package upstream

import (
	"os"
	"path/filepath"
	"testing"
)

// 手编覆盖（source=manual）压过上游动态值：上游对 deepseek 报 128k，用户手编
// 384k 后 /v1/models 必须报 384k。
func TestManualCapOverridesRemote(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	if err := os.WriteFile(path, []byte(`{
		"deepseek-v4.1-flash": {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	loadModelCatalogAt(path)

	if got := ContextWindowListingV4("deepseek-v4.1-flash", 1000000, nil); got != 1000000 {
		t.Fatalf("context = %d, want 1000000", got)
	}
	got, ok := MaxOutputTokensListingV4("deepseek-v4.1-flash", 128000, nil)
	if !ok || got != 384000 {
		t.Fatalf("max output = %d (ok=%v), want 384000", got, ok)
	}
	// 没有手编条目的模型仍以上游为准。
	if got, ok := MaxOutputTokensListingV4("glm-5.3-flash", 128000, nil); !ok || got != 128000 {
		t.Fatalf("glm max output = %d (ok=%v), want upstream 128000", got, ok)
	}
}

// models.dev 回填不得覆盖手编条目（否则手编值在第一次拉取后静默失效）。
func TestModelCatalogPutKeepsManual(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	if err := os.WriteFile(path, []byte(`{
		"deepseek-v4.1-flash": {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	loadModelCatalogAt(path)

	modelCatalogPut("deepseek-v4.1-flash", 1000000, 128000)
	if got, ok := MaxOutputTokensListingV4("deepseek-v4.1-flash", 128000, nil); !ok || got != 384000 {
		t.Fatalf("manual entry overwritten: max output = %d (ok=%v), want 384000", got, ok)
	}
}

// 隐藏付费变体（如 deepseek-v4.1-flash-sg 被 clientPick 从公开目录滤掉）以及冷启动尚未拉取目录时，
// manual 条目仍须对 injectCatalogMaxTokens 与 OutputBudgetFloor 生效。
func TestManualCapAppliesToHiddenVariantAndColdStart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	if err := os.WriteFile(path, []byte(`{
		"deepseek-v4.1-flash-sg": {"context_length": 1000000, "max_output_tokens": 384000, "source": "manual"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	loadModelCatalogAt(path)

	// 1. 冷启动（maxOut 为 nil）下 injectCatalogMaxTokens 与 outputFloor 都能命中手编值。
	obj := map[string]any{"model": "deepseek-v4.1-flash-sg"}
	injectCatalogMaxTokens(obj, nil)
	if got, _ := obj["max_tokens"].(int64); got != 384000 {
		t.Fatalf("cold-start injectCatalogMaxTokens = %v, want 384000", obj["max_tokens"])
	}
	if floor := outputFloor("max", "deepseek-v4.1-flash-sg", nil); floor != 16384 {
		t.Fatalf("cold-start outputFloor = %d, want 16384", floor)
	}

	// 2. storeModelMaxOut 收到不含 -sg 的公开目录 infos 后，modelMaxOut 快照仍包含 -sg=384000。
	c := New()
	c.storeModelMaxOut("global", []ModelInfo{{ID: "deepseek-v4.1-flash", MaxTokens: 128000}})
	if floor := c.OutputBudgetFloor("global", "deepseek-v4.1-flash-sg", "high"); floor != 8192 {
		t.Fatalf("OutputBudgetFloor for -sg = %d, want 8192", floor)
	}
}
