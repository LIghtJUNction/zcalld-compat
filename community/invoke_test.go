package community

import "testing"

func TestPromptFieldSurvivesWithoutExposingCredentialFields(t *testing.T) {
	prompt := map[string]any{"key": "region", "type": "select", "access": "private", "options": []any{
		map[string]any{"label": "Europe", "value": "eu", "token": "private"},
	}}
	reply := map[string]any{"prompt": prompt, "key": "private"}
	publicResult("prompt", reply)
	if prompt["field"] != "region" || prompt["key"] != nil || prompt["access"] != nil || reply["key"] != nil {
		t.Fatal("prompt metadata and credentials did not remain separate")
	}
	choices := prompt["options"].([]map[string]string)
	if len(choices) != 1 || choices[0]["value"] != "eu" || choices[0]["token"] != "" {
		t.Fatal("prompt choices did not preserve only the public selection fields")
	}
}
