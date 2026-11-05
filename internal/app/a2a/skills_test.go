package a2a

// 对应 rs src/a2a/skills.rs 的单测。

import (
	"ir-agent-server/internal/domain/position"
	"strings"
	"testing"
)

func TestA2AEveryPresetRoleHasAProfile(t *testing.T) {
	for _, descriptor := range position.Descriptors() {
		found := false
		for _, profile := range a2aSkillProfiles {
			if profile.roleID == descriptor.ID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("role %s has no A2A skill profile", descriptor.ID)
		}
	}
}

func TestA2APresetRoleMapsToSkillWithTagsAndContractFields(t *testing.T) {
	skills := A2ASkillsFor("risk_manager", "内部风控闸门")
	if len(skills) != 1 {
		t.Fatalf("len = %d, want 1", len(skills))
	}
	skill := skills[0]
	if skill.ID != "risk_manager" {
		t.Errorf("id = %q", skill.ID)
	}
	if !strings.Contains(skill.Name, "风控经理") {
		t.Errorf("name = %q", skill.Name)
	}
	if len(skill.Tags) == 0 {
		t.Error("tags are required in v1.0")
	}
	if !strings.Contains(skill.Description, "内部风控闸门") {
		t.Errorf("description = %q", skill.Description)
	}
	if !strings.Contains(skill.Description, "gate") {
		t.Errorf("description missing gate: %q", skill.Description)
	}
	if !strings.Contains(skill.Description, "checks") {
		t.Errorf("description missing checks: %q", skill.Description)
	}
}

func TestA2AEmptyPromptDefaultsToFundamentalAnalyst(t *testing.T) {
	skills := A2ASkillsFor("", "")
	if skills[0].ID != "fundamental_analyst" {
		t.Errorf("id = %q", skills[0].ID)
	}
	if !strings.Contains(skills[0].Description, "assessments") {
		t.Errorf("description = %q", skills[0].Description)
	}
}

func TestA2ACustomPromptFallsBackToCustomSkill(t *testing.T) {
	skills := A2ASkillsFor("你是一个特殊的分析助手", "")
	if skills[0].ID != a2aCustomSkillID {
		t.Errorf("id = %q, want %q", skills[0].ID, a2aCustomSkillID)
	}
	if len(skills[0].Tags) == 0 {
		t.Error("tags must not be empty")
	}
}

func TestA2ADescriptionIsNeverEmpty(t *testing.T) {
	for _, skill := range A2ASkillsFor("technical_analyst", "") {
		if strings.TrimSpace(skill.Description) == "" {
			t.Error("description must not be empty")
		}
	}
}
