package app

import (
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/routing"
)

// decisionBounds projects the decisions: config block onto the proxy's
// typed-decision classifier bounds. It is per generation, so a reload applies
// new bounds to requests on the next generation.
func decisionBounds(d config.DecisionsConfig) routing.DecisionBounds {
	return routing.DecisionBounds{
		MaxQuestions:         d.MaxQuestions,
		MaxQuestionNameBytes: d.MaxQuestionNameBytes,
		MaxChoiceOptions:     d.MaxChoiceOptions,
		MaxScoreLevels:       d.MaxScoreLevels,
		MaxNesting:           d.MaxNesting,
	}
}
