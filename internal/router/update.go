package router

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/freegle/splitter/internal/config"
	"github.com/freegle/splitter/internal/store"
)

// minDivergenceN and divergencePoints are the per-exact-version divergence
// detection thresholds (DESIGN.md "Model families": "a specific version
// has n >= 10 and its agreement rate sits more than 10 points below the
// family aggregate"). The exact-version key used here is the local
// (backend) model: DESIGN.md motivates this by a local model version bump
// ("qwen2.5-coder:7b" -> "qwen3-coder:7b"), the axis a live routing
// decision actually chooses, so that is what is tracked per exact version;
// see DECISIONS.md.
const (
	minDivergenceN   = 10
	divergencePoints = 0.10
)

// VersionDivergence is one exact local-model version flagged, within a
// (category, families) group, for sitting materially below the family
// aggregate's agreement rate.
type VersionDivergence struct {
	Version       string
	N             int
	AgreementRate float64
	FamilyRate    float64
}

// CategoryStats is one (category, families) router_state row as computed
// by Update, plus the version divergences (if any) that caused its stats
// to be recomputed from a subset of the data.
type CategoryStats struct {
	Category       string
	Families       string
	N              int
	Agreed         int
	WilsonLB       float64
	Routable       bool
	DisabledReason string
	Diverged       []VersionDivergence
}

// UpdateResult is the full output of one `splitter router update` run, one
// row per (category, families) group found in the data, sorted by category
// then families for deterministic printing.
type UpdateResult struct {
	Rows []CategoryStats
}

// Update recomputes every router_state row from verifications joined to
// features, grouped by (category, family pair): see DESIGN.md
// "internal/router" and "Model families". For each group it also computes
// per-exact-local-model-version agreement; a version with at least
// minDivergenceN rows whose agreement rate sits more than divergencePoints
// below the group's family aggregate is flagged, and the group's stats are
// recomputed from the flagged version(s)' rows only (pooled, when more
// than one diverges), which both updates n/agreed/wilson_lb to the fresher
// numbers and disables routing for the category (disabled_reason records
// the flag and the recomputed basis, and Routable always requires an empty
// disabled_reason). Every group's router_state row is upserted before
// Update returns.
func Update(db *sql.DB, cfg *config.Config) (*UpdateResult, error) {
	rows, err := store.DecidedVerificationsForRouter(db)
	if err != nil {
		return nil, fmt.Errorf("loading decided verifications: %w", err)
	}

	type groupKey struct {
		category string
		families string
	}
	groups := map[groupKey][]store.VerificationForRouter{}
	var order []groupKey
	for _, r := range rows {
		key := groupKey{
			category: Category(r.TurnType, r.Subsystem),
			families: FamilyPair(r.FrontierModel, r.LocalModel, cfg.Families),
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], r)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	result := &UpdateResult{}

	for _, key := range order {
		grp := groups[key]
		stats := computeCategoryStats(key.category, key.families, grp, cfg.Router.MinN, cfg.Router.MinWilsonLB)

		if err := store.UpsertRouterState(db, store.RouterStateRow{
			Category:       stats.Category,
			Families:       stats.Families,
			N:              stats.N,
			Agreed:         stats.Agreed,
			WilsonLB:       stats.WilsonLB,
			Routable:       stats.Routable,
			DisabledReason: stats.DisabledReason,
			UpdatedTS:      now,
		}); err != nil {
			return nil, fmt.Errorf("writing router state for %s/%s: %w", key.category, key.families, err)
		}
		result.Rows = append(result.Rows, stats)
	}

	sort.Slice(result.Rows, func(i, j int) bool {
		if result.Rows[i].Category != result.Rows[j].Category {
			return result.Rows[i].Category < result.Rows[j].Category
		}
		return result.Rows[i].Families < result.Rows[j].Families
	})
	return result, nil
}

// computeCategoryStats aggregates one (category, families) group's rows,
// detects per-exact-local-model-version divergence within it, and applies
// the recompute-from-diverged-rows rule described on Update.
func computeCategoryStats(category, families string, grp []store.VerificationForRouter, minN int, minWilsonLB float64) CategoryStats {
	n, agreed := countAgreement(grp)
	familyRate := rate(agreed, n)

	byVersion := map[string][]store.VerificationForRouter{}
	var versionOrder []string
	for _, r := range grp {
		if _, ok := byVersion[r.LocalModel]; !ok {
			versionOrder = append(versionOrder, r.LocalModel)
		}
		byVersion[r.LocalModel] = append(byVersion[r.LocalModel], r)
	}

	var diverged []VersionDivergence
	for _, version := range versionOrder {
		vrows := byVersion[version]
		vn, vagreed := countAgreement(vrows)
		if vn < minDivergenceN {
			continue
		}
		vRate := rate(vagreed, vn)
		if familyRate-vRate > divergencePoints {
			diverged = append(diverged, VersionDivergence{
				Version:       version,
				N:             vn,
				AgreementRate: vRate,
				FamilyRate:    familyRate,
			})
		}
	}
	sort.Slice(diverged, func(i, j int) bool { return diverged[i].Version < diverged[j].Version })

	disabledReason := ""
	if len(diverged) > 0 {
		var pooled []store.VerificationForRouter
		for _, d := range diverged {
			pooled = append(pooled, byVersion[d.Version]...)
		}
		n, agreed = countAgreement(pooled)
		disabledReason = formatDivergenceReason(diverged)
	}

	lb := WilsonLowerBound(agreed, n, WilsonZ95)
	return CategoryStats{
		Category:       category,
		Families:       families,
		N:              n,
		Agreed:         agreed,
		WilsonLB:       lb,
		Routable:       Routable(n, lb, disabledReason, minN, minWilsonLB),
		DisabledReason: disabledReason,
		Diverged:       diverged,
	}
}

// formatDivergenceReason renders diverged as an auditable disabled_reason
// string, e.g. "divergent_version:qwen3-coder-v2(n=12,rate=41.7%,family=91.2%)".
func formatDivergenceReason(diverged []VersionDivergence) string {
	s := "divergent_version:"
	for i, d := range diverged {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%s(n=%d,rate=%.1f%%,family=%.1f%%)", d.Version, d.N, d.AgreementRate*100, d.FamilyRate*100)
	}
	return s
}

func countAgreement(rows []store.VerificationForRouter) (n, agreed int) {
	n = len(rows)
	for _, r := range rows {
		if r.Agree {
			agreed++
		}
	}
	return n, agreed
}

func rate(agreed, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(agreed) / float64(n)
}
