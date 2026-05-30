package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var ErrSkillNotFound = errors.New("skill not found in registry")

type skillResponseFromL1 struct {
	SkillID          string                 `json:"skill_id"`
	Version          string                 `json:"version"`
	Name             string                 `json:"name"`
	Description      string                 `json:"description"`
	Category         string                 `json:"category"`
	ModelRecommended *modelRecommended      `json:"model_recommended"`
	PromptContent    string                 `json:"prompt_content"`
	OutputSchema     map[string]interface{} `json:"output_schema"`
	Visibility       string                 `json:"visibility"`
	Status           string                 `json:"status"`
	ScriptsPath      string                 `json:"scripts_path"`
	TemplatesPath    string                 `json:"templates_path"`
	ExamplesPath     string                 `json:"examples_path"`
}

type modelRecommended struct {
	Primary  string   `json:"primary"`
	Fallback []string `json:"fallback"`
}

func FetchSkillFromL1(ctx context.Context, registryURL, skillID string) (SkillDef, error) {
	url := fmt.Sprintf("%s/api/skill/%s", registryURL, skillID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return SkillDef{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Internal-Token", "dev-token-12345")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return SkillDef{}, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusNotFound {
			return SkillDef{}, fmt.Errorf("%w: %s", ErrSkillNotFound, skillID)
		}
		return SkillDef{}, fmt.Errorf("registry returned %d: %s", resp.StatusCode, string(body))
	}

	var l1 skillResponseFromL1
	if err := json.NewDecoder(resp.Body).Decode(&l1); err != nil {
		return SkillDef{}, fmt.Errorf("decode response: %w", err)
	}

	if l1.PromptContent == "" {
		return SkillDef{}, fmt.Errorf("skill %s has no prompt_content", skillID)
	}

	schemaStr := ""
	if l1.OutputSchema != nil {
		schemaBytes, _ := json.Marshal(l1.OutputSchema)
		schemaStr = string(schemaBytes)
	}

	model := ""
	if l1.ModelRecommended != nil {
		model = l1.ModelRecommended.Primary
	}

	return SkillDef{
		ID:               l1.SkillID,
		Name:             l1.Name,
		Description:      l1.Description,
		Category:         l1.Category,
		ModelRecommended: model,
		OutputSchema:     schemaStr,
		RawContent:       l1.PromptContent,
	}, nil
}
