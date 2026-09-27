package webhook_test

import (
	"maps"
	"strings"
	"testing"
)

// TestMain refuses to start when the chart routes a webhook to a path the
// manager does not register, or the manager registers one nothing routes to.
// A check that has never refused anything has not been measured, so each row
// here is a way the two drift apart and the check has to name it.
func TestThePathCheckRefusesADisagreement(t *testing.T) {
	plane.Require(t)
	if problems := unservedManagerPaths(chartMutating, chartValidating, servedPaths()); len(problems) != 0 {
		t.Fatalf("the check refused the chart as rendered: %v", problems)
	}

	tests := []struct {
		name     string
		problems func() []string
		want     string
	}{
		{
			name: "a chart entry routed to a renamed path",
			problems: func() []string {
				validating := chartValidating.DeepCopy()
				for index := range validating.Webhooks {
					if validating.Webhooks[index].Name == controllerWriteWebhook {
						renamed := validateControllerWritePath + "s"
						validating.Webhooks[index].ClientConfig.Service.Path = &renamed
					}
				}
				return unservedManagerPaths(chartMutating, validating, servedPaths())
			},
			want: controllerWriteWebhook + " routes to " + validateControllerWritePath + "s",
		},
		{
			name: "a handler no chart entry routes to",
			problems: func() []string {
				served := maps.Clone(servedPaths())
				served["/validate-unrouted"] = true
				return unservedManagerPaths(chartMutating, chartValidating, served)
			},
			want: "the manager serves /validate-unrouted",
		},
		{
			// Without the annotation no entry is recognized as the manager's,
			// and a check that looked at nothing must not pass.
			name: "the configuration no longer names the manager's Service",
			problems: func() []string {
				mutating := chartMutating.DeepCopy()
				validating := chartValidating.DeepCopy()
				delete(mutating.Annotations, "operator.ptah.run/webhook-service-name")
				delete(validating.Annotations, "operator.ptah.run/webhook-service-name")
				return unservedManagerPaths(mutating, validating, servedPaths())
			},
			want: "the manager serves " + validateControllerWritePath + ", and no chart entry routes to it",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			problems := test.problems()
			if !strings.Contains(strings.Join(problems, "\n"), test.want) {
				t.Fatalf("the check reported %v, want a problem containing %q", problems, test.want)
			}
		})
	}
}
