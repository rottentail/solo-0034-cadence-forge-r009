package journal

import (
	"cadenceforge/internal/domain"
)

// migrate upgrades state written before frozen submission identities existed.
// Older renders kept the complete frozen plan, so the identity can be derived
// without changing any committed fact. Segmented deliveries did not exist at
// schema 1 and are never synthesized here; an unknown ladder fails validation.
func migrate(state *domain.State) error {
	for id, render := range state.Renders {
		if render.FrozenSubmission != "" {
			continue
		}
		render.FrozenSubmission = domain.SubmissionDigest(render.Sheet, render.Recipe, render.Sources, render.Plan)
		state.Renders[id] = render
	}
	return nil
}
