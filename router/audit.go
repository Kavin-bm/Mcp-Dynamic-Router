package router

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type DescriptionPolicy struct {
	RequireDomain      bool
	RequiredSections   []string
	MinimumDescription int
}

func DefaultDescriptionPolicy() DescriptionPolicy {
	return DescriptionPolicy{
		RequireDomain:      true,
		RequiredSections:   []string{"purpose", "invoke when", "parameters", "limitations", "example"},
		MinimumDescription: 120,
	}
}

type AuditIssue struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type AuditReport struct {
	ToolID string       `json:"tool_id"`
	Score  int          `json:"score"`
	Issues []AuditIssue `json:"issues"`
}

var headingPattern = regexp.MustCompile(`(?im)^\s*(domain|purpose|invoke when|parameters|limitations|example)\s*:\s*`)

var sectionRegexCache sync.Map

func getSectionRegex(section string) *regexp.Regexp {
	if cached, ok := sectionRegexCache.Load(section); ok {
		return cached.(*regexp.Regexp)
	}
	re := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(section) + `\s*:`)
	sectionRegexCache.Store(section, re)
	return re
}

func AuditTool(tool Tool, policy DescriptionPolicy) AuditReport {
	report := AuditReport{ToolID: tool.ID, Score: 100}
	description := strings.TrimSpace(tool.Description)
	lower := strings.ToLower(description)

	if len(description) < policy.MinimumDescription {
		report.add("warning", "description.too_short", fmt.Sprintf("description should be at least %d characters", policy.MinimumDescription), 10)
	}
	for _, section := range policy.RequiredSections {
		if !getSectionRegex(section).MatchString(lower) {
			report.add("error", "section.missing."+strings.ReplaceAll(section, " ", "_"), "missing required section: "+section, 12)
		}
	}
	if policy.RequireDomain && strings.TrimSpace(tool.Domain) == "" && !getSectionRegex("domain").MatchString(lower) {
		report.add("error", "domain.missing", "set Tool.Domain or add a Domain: section", 12)
	}

	var schema map[string]any
	if len(tool.InputSchema) == 0 || json.Unmarshal(tool.InputSchema, &schema) != nil {
		report.add("error", "schema.invalid", "input schema must be a valid JSON object", 20)
	} else {
		properties, _ := schema["properties"].(map[string]any)
		for name, raw := range properties {
			property, _ := raw.(map[string]any)
			if strings.TrimSpace(asString(property["type"])) == "" {
				report.add("error", "parameter.type_missing", fmt.Sprintf("parameter %q has no type", name), 5)
			}
			if strings.TrimSpace(asString(property["description"])) == "" {
				report.add("error", "parameter.description_missing", fmt.Sprintf("parameter %q has no description", name), 5)
			}
		}
	}

	sort.SliceStable(report.Issues, func(i, j int) bool { return report.Issues[i].Code < report.Issues[j].Code })
	if report.Score < 0 {
		report.Score = 0
	}
	return report
}

func (r *AuditReport) add(severity, code, message string, penalty int) {
	r.Issues = append(r.Issues, AuditIssue{Severity: severity, Code: code, Message: message})
	r.Score -= penalty
}

func ExtractDomain(tool Tool) string {
	if domain := strings.TrimSpace(tool.Domain); domain != "" {
		return normalizeLabel(domain)
	}
	matches := headingPattern.FindAllStringSubmatchIndex(tool.Description, -1)
	for i, match := range matches {
		if strings.ToLower(tool.Description[match[2]:match[3]]) != "domain" {
			continue
		}
		end := len(tool.Description)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		return normalizeLabel(strings.TrimSpace(tool.Description[match[1]:end]))
	}
	if tool.Server != "" {
		return normalizeLabel(tool.Server)
	}
	return "uncategorized"
}

func normalizeLabel(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(value)), "-")
}

func asString(value any) string {
	result, _ := value.(string)
	return result
}
