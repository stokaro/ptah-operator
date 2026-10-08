package main

import (
	"errors"

	"gopkg.in/yaml.v3"
)

func verifyReleaseDispatch(node yaml.Node) error {
	var dispatch struct {
		Inputs map[string]struct {
			Type     string   `yaml:"type"`
			Required bool     `yaml:"required"`
			Default  string   `yaml:"default"`
			Options  []string `yaml:"options"`
		} `yaml:"inputs"`
	}
	if err := node.Decode(&dispatch); err != nil {
		return err
	}
	action, digest := dispatch.Inputs["action"], dispatch.Inputs["manifest_sha256"]
	if len(dispatch.Inputs) != 2 || action.Type != "choice" || !action.Required || action.Default != "smoke" ||
		!equalStringSet(action.Options, []string{"smoke", "prepare", "publish"}) ||
		digest.Type != "string" || digest.Required || digest.Default != "" || len(digest.Options) != 0 {
		return errors.New("manual release dispatch must default to smoke and require an explicit action and qualified manifest for publication")
	}
	return nil
}

func verifyReleasePreparation(smoke []workflowStep, steps map[string]workflowStep) error {
	smokeSteps, err := stepsByID(smoke)
	if err != nil {
		return err
	}
	verification, err := requireStep(smokeSteps, "verify-release")
	if err != nil {
		return err
	}
	if !equalStringMap(verification.Env, map[string]string{
		"RELEASE_ACTION":            "${{ inputs.action }}",
		"QUALIFIED_MANIFEST_SHA256": "${{ inputs.manifest_sha256 }}",
	}) {
		return errors.New("manual smoke validation must read the requested action without shell interpolation")
	}
	if err := requireRunBindings(smokeSteps, "verify-release",
		`if [[ "$GITHUB_EVENT_NAME" == workflow_dispatch ]]; then`,
		`[[ "$RELEASE_ACTION" == smoke && -z "$QUALIFIED_MANIFEST_SHA256" ]] || {`,
		"  exit 1\n", "go run ./hack/releaseverify"); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "release", `case "$RELEASE_ACTION" in
  prepare) [[ -z "$QUALIFIED_MANIFEST_SHA256" ]] ;;
  publish) [[ "$QUALIFIED_MANIFEST_SHA256" =~ ^[0-9a-f]{64}$ ]] ;;
  *) echo 'release action must be prepare or publish' >&2; exit 1 ;;
esac`); err != nil {
		return err
	}
	if err := requireRunBindings(steps, "transaction", `if [[ "$RELEASE_ACTION" == publish ]]; then
  [[ "$release_state" == recover || "$release_state" == published ]] || {
    echo 'prepare and qualify a complete signed draft before requesting publication' >&2
    exit 1
  }
  [[ "$(sha256sum "$state_path" | awk '{print $1}')" == "$QUALIFIED_MANIFEST_SHA256" ]] || {
    echo 'the requested manifest differs from the existing transaction' >&2
    exit 1
  }
fi`); err != nil {
		return err
	}
	return requireRunBindings(steps, "publish-release", `if [[ "$mode" != published ]]; then
  if [[ "$RELEASE_ACTION" == prepare ]]; then
    manifest_sha256="$(sha256sum "$gate_dir/release-manifest.txt" | awk '{print $1}')"
    printf 'Signed draft prepared; official release remains unpublished. Manifest SHA-256: %s\n' "$manifest_sha256" >> "$GITHUB_STEP_SUMMARY"
    exit 0
  fi
  [[ "$RELEASE_ACTION" == publish ]] || { echo 'publication was not requested' >&2; exit 1; }
  [[ "$(sha256sum "$gate_dir/release-manifest.txt" | awk '{print $1}')" == "$QUALIFIED_MANIFEST_SHA256" ]] || {
    echo 'the final manifest differs from the qualified transaction' >&2
    exit 1
  }
  gh release edit "$GITHUB_REF_NAME" --draft=false --latest=false`)
}
