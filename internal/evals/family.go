package evals

import "strings"

// Family normalises an exact model id to its family key, per DESIGN.md
// "Model families": strip date suffixes and generation-number segments,
// keep variant words and parameter-size tags, lowercase. overrides is
// config.Config.Families, an exact-model-id override checked first.
//
// This is a local, standalone implementation: internal/router (Phase 4,
// not yet built) is DESIGN.md's designated owner of this exact algorithm
// for router_state's family-pair scoping. The eval library needs the same
// normalisation independently for its [model_cutoffs] contamination guard,
// so it is duplicated here rather than creating a build dependency on a
// component that does not exist yet; when internal/router lands, its
// Family function and this one should produce identical results for every
// model id this codebase touches (both are validated against the same
// DESIGN.md examples).
func Family(model string, overrides map[string]string) string {
	if model == "" {
		return ""
	}
	if fam, ok := overrides[model]; ok {
		return fam
	}

	m := model
	if idx := strings.LastIndex(m, "/"); idx >= 0 {
		m = m[idx+1:]
	}
	m = strings.ToLower(m)
	m = stripTrailingDateSuffix(m)

	base, tag, hasTag := strings.Cut(m, ":")
	normalized := normalizeModelSegments(base)
	if hasTag {
		return normalized + ":" + tag
	}
	return normalized
}

// stripTrailingDateSuffix removes a trailing "-YYYYMMDD" or "@YYYYMMDD"
// segment (8 digits), if present.
func stripTrailingDateSuffix(m string) string {
	for _, sep := range []string{"-", "@"} {
		idx := strings.LastIndex(m, sep)
		if idx < 0 {
			continue
		}
		suffix := m[idx+1:]
		if len(suffix) == 8 && isAllDigits(suffix) {
			return m[:idx]
		}
	}
	return m
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// normalizeModelSegments splits s on "-" and, for each segment, strips a
// trailing run of digit/dot characters (a fused-on generation number like
// the "2.5" in "qwen2.5"), dropping the segment entirely when nothing is
// left (a segment that was purely a version number, like "5" or "4.5").
// A segment with no trailing digit/dot run (including one ending in a
// letter right after a digit, like "4o" or "32b") is kept unchanged: this
// is what keeps parameter-size tags and letter-suffixed product numbers
// like "gpt-4o-mini" stable.
func normalizeModelSegments(s string) string {
	parts := strings.Split(s, "-")
	kept := parts[:0]
	for _, p := range parts {
		if stripped := stripTrailingVersionRun(p); stripped != "" {
			kept = append(kept, stripped)
		}
	}
	return strings.Join(kept, "-")
}

func stripTrailingVersionRun(s string) string {
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if c == '.' || (c >= '0' && c <= '9') {
			i--
			continue
		}
		break
	}
	return s[:i]
}

// ModelCutoff looks up model's training data cutoff ("YYYY-MM") in cutoffs:
// an exact model id match wins over a family match (Family(model,
// familyOverrides)). ok is false when neither is configured.
func ModelCutoff(model string, cutoffs, familyOverrides map[string]string) (cutoff string, ok bool) {
	if c, exists := cutoffs[model]; exists {
		return c, true
	}
	fam := Family(model, familyOverrides)
	if c, exists := cutoffs[fam]; exists {
		return c, true
	}
	return "", false
}

// Cutoff segment labels for eval run's scorecard split.
const (
	SegmentPreCutoff     = "pre_cutoff"
	SegmentPostCutoff    = "post_cutoff"
	SegmentUnknownCutoff = "unknown_cutoff"
)

// CutoffSegment classifies taskDate (an RFC3339 timestamp or "YYYY-MM-DD"
// date) against model's configured cutoff: pre_cutoff (memorisation
// suspect) when the task predates the cutoff month, post_cutoff (trusted)
// otherwise, unknown_cutoff when no cutoff is configured for model or
// taskDate is empty. DESIGN.md: "Live-harvested tasks are post-cutoff by
// construction" holds automatically here since their task_date is the
// capture timestamp, always at or after "now".
func CutoffSegment(taskDate, model string, cutoffs, familyOverrides map[string]string) string {
	cutoff, ok := ModelCutoff(model, cutoffs, familyOverrides)
	if !ok || taskDate == "" {
		return SegmentUnknownCutoff
	}
	taskYM := taskDate
	if len(taskYM) >= 7 {
		taskYM = taskYM[:7]
	}
	if taskYM < cutoff {
		return SegmentPreCutoff
	}
	return SegmentPostCutoff
}
