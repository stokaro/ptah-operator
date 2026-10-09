package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Preparation binds the protected master ref without reserving a release tag.
// Publication continues to require the live tag to identify the same commit.
func verifyReleaseSource(ctx context.Context, client *http.Client, apiURL, repository, tag, ref, sourceSHA, action, token string) error {
	if ref == "refs/tags/"+tag {
		return verifyGitHubTagIdentity(ctx, client, apiURL, repository, tag, sourceSHA, token)
	}
	if ref != preparationSourceRef || action != "prepare" {
		return errors.New("only master preparation or the exact release tag may produce release artifacts")
	}
	base, err := releaseAPIBase(client, apiURL, repository, token)
	if err != nil {
		return err
	}
	if !commitPattern.MatchString(sourceSHA) || !semanticVersionPattern.MatchString(strings.TrimPrefix(tag, "v")) || !strings.HasPrefix(tag, "v") {
		return errors.New("preparation requires an exact source commit and version tag name")
	}
	object, err := fetchGitObject(ctx, client, base, token, "repos/"+repository+"/git/ref/heads/master")
	if err != nil {
		return err
	}
	if object.Type != "commit" || object.SHA != sourceSHA {
		return errors.New("preparation source differs from the current master commit")
	}
	_, status, err := readReleaseAPI(ctx, client, base, token, "repos/"+repository+"/git/ref/tags/"+tag)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("untagged preparation requires an absent release tag; GitHub returned HTTP %d", status)
	}
	return nil
}

// The releases list includes authenticated drafts whose future tag does not
// exist yet. API failures are errors, never evidence of a fresh transaction.
func readGitHubRelease(ctx context.Context, client *http.Client, apiURL, repository, tag, sourceSHA, token string) ([]byte, error) {
	base, err := releaseAPIBase(client, apiURL, repository, token)
	if err != nil {
		return nil, err
	}
	if tag == "" || !commitPattern.MatchString(sourceSHA) {
		return nil, errors.New("release lookup requires a tag name and exact source SHA")
	}
	var found json.RawMessage
	for page := 1; ; page++ {
		body, status, err := readReleaseAPI(ctx, client, base, token, fmt.Sprintf("repos/%s/releases?per_page=100&page=%d", repository, page))
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("release lookup: GitHub returned HTTP %d", status)
		}
		var releases []json.RawMessage
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, err
		}
		if releases == nil {
			return nil, errors.New("release lookup requires a JSON array")
		}
		for _, raw := range releases {
			var release struct {
				Tag    string `json:"tag_name"`
				Target string `json:"target_commitish"`
				Draft  bool   `json:"draft"`
			}
			if err := json.Unmarshal(raw, &release); err != nil {
				return nil, err
			}
			if release.Tag != tag {
				continue
			}
			if found != nil {
				return nil, errors.New("multiple releases name the selected tag")
			}
			if release.Draft && release.Target != sourceSHA {
				return nil, errors.New("draft target must be the exact selected source commit")
			}
			found = raw
		}
		if len(releases) < 100 {
			break
		}
	}
	if found == nil {
		return []byte("null"), nil
	}
	return found, nil
}

func releaseAPIBase(client *http.Client, apiURL, repository, token string) (*url.URL, error) {
	if client == nil || repository != repositoryName || token == "" {
		return nil, errors.New("release API requires a client, the release repository and an authenticated token")
	}
	base, err := url.Parse(apiURL)
	if err != nil || base.Scheme == "" || base.Host == "" || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("invalid release API URL")
	}
	return base, nil
}

func readReleaseAPI(ctx context.Context, client *http.Client, base *url.URL, token, path string) ([]byte, int, error) {
	relative, err := url.Parse(path)
	if err != nil {
		return nil, 0, err
	}
	endpoint := *base
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/" + relative.Path
	endpoint.RawQuery = relative.RawQuery
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("read release API: %w", err)
	}
	defer response.Body.Close()
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, 0, err
	}
	if len(body) > limit {
		return nil, 0, errors.New("release API response exceeds its bound")
	}
	return body, response.StatusCode, nil
}
