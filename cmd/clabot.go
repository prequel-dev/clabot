package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v58/github"
	"github.com/rs/zerolog/log"
	"golang.org/x/oauth2"
)

type cfg struct {
	RepoOwner      string // e.g. "your-org"
	RepoName       string // e.g. "awesome-project"
	EventName      string // pull_request or issue_comment
	EventPath      string // path to the JSON payload created by Actions
	SignersPath    string // path in repo: "cla-signers.txt"
	Token          string // GITHUB_TOKEN injected by Actions
	GoogleSheetUrl string // Path to public Google spreadsheet with signers
	CommentMsg     string // Message to post as a comment
	IgnoreAuthors  map[string]struct{}
}

func fromEnv() cfg {
	repo := os.Getenv("GITHUB_REPOSITORY") // "<owner>/<repo>"
	s := strings.Split(repo, "/")
	c := cfg{
		RepoOwner:      s[0],
		RepoName:       s[1],
		EventName:      os.Getenv("GITHUB_EVENT_NAME"),
		EventPath:      os.Getenv("GITHUB_EVENT_PATH"),
		SignersPath:    os.Getenv("SIGNERS_PATH"),
		Token:          os.Getenv("GITHUB_TOKEN"),
		GoogleSheetUrl: os.Getenv("GOOGLE_SHEET_URL"),
		CommentMsg:     os.Getenv("COMMENT_MSG"),
		IgnoreAuthors:  make(map[string]struct{}),
	}

	raw := os.Getenv("BOT_IGNORE_AUTHORS")
	if raw == "" {
		raw = "github-actions[bot]"
	}
	for _, a := range strings.Split(raw, ",") {
		c.IgnoreAuthors[strings.ToLower(strings.TrimSpace(a))] = struct{}{}
	}

	if c.CommentMsg == "" {
		c.CommentMsg = "Please sign the CLA and then comment `@cla-bot check` on this PR."
	}

	return c
}

func newGHClient(token string) *github.Client {
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	return github.NewClient(oauth2.NewClient(ctx, ts))
}

func loadSignersFromGoogleSheet(ctx context.Context, csvURL string) (map[string]struct{}, error) {
	if csvURL == "" {
		return nil, errors.New("csv url not provided")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, csvURL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("google sheets returned %s", resp.Status)
	}

	rdr := csv.NewReader(resp.Body)
	rows, err := rdr.ReadAll()
	if err != nil {
		return nil, err
	}

	signers := make(map[string]struct{}, len(rows))
	for i, row := range rows {
		if i == 0 { // skip header row
			continue
		}
		if len(row) == 0 {
			continue
		}
		login := strings.ToLower(strings.TrimSpace(row[1]))
		if login != "" {
			signers[login] = struct{}{}
		}
	}

	for k := range signers {
		log.Info().Str("signer", k).Msg("Google Sheet CLA signer")
	}

	return signers, nil
}

func loadSignersGithub(ctx context.Context, gh *github.Client, c cfg, ref string) (map[string]struct{}, error) {
	file, _, _, err := gh.Repositories.GetContents(ctx, c.RepoOwner, c.RepoName, c.SignersPath, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, err
	}

	s, err := file.GetContent()
	if err != nil {
		return nil, err
	}

	set := make(map[string]struct{})
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			set[strings.ToLower(line)] = struct{}{}
		}
	}

	for k := range set {
		log.Info().Str("signer", k).Msg("Github CLA signer")
	}

	return set, nil
}

func loadSigners(ctx context.Context, gh *github.Client, c cfg, ref string) (map[string]struct{}, error) {
	merged := make(map[string]struct{})

	if c.GoogleSheetUrl != "" {
		if m, err := loadSignersFromGoogleSheet(ctx, c.GoogleSheetUrl); err != nil {
			return nil, fmt.Errorf("sheet: %w", err)
		} else {
			for k := range m {
				merged[k] = struct{}{}
			}
		}
	}

	if c.SignersPath != "" {
		if m, err := loadSignersGithub(ctx, gh, c, ref); err != nil {
			return nil, fmt.Errorf("repo file: %w", err)
		} else {
			for k := range m {
				merged[k] = struct{}{}
			}
		}
	}

	return merged, nil
}

func postStatus(ctx context.Context, gh *github.Client, c cfg, sha, state, description string) {
	log.Info().
		Str("sha", sha).
		Str("state", state).
		Str("description", description).
		Msg("Posting status")

	_, _, _ = gh.Repositories.CreateStatus(ctx, c.RepoOwner, c.RepoName, sha, &github.RepoStatus{
		State:       github.String(state), // "success" | "failure"
		Description: github.String(description),
		Context:     github.String("CLA check"),
	})
}

func postComment(ctx context.Context, gh *github.Client, c cfg, prNumber int, body string) {
	_, _, _ = gh.Issues.CreateComment(ctx, c.RepoOwner, c.RepoName, prNumber, &github.IssueComment{Body: github.String(body)})
}

func handlePullRequest(ctx context.Context, gh *github.Client, c cfg) error {
	var ev github.PullRequestEvent
	if err := parseEvent(c.EventPath, &ev); err != nil {
		return err
	}

	pr := ev.GetPullRequest()
	author := strings.ToLower(pr.GetUser().GetLogin())
	sha := pr.GetHead().GetSHA()

	signers, err := loadSigners(ctx, gh, c, pr.GetBase().GetRef())
	if err != nil {
		return err
	}

	if _, ok := signers[author]; ok {
		postStatus(ctx, gh, c, sha, "success", "CLA signed ✔️")
	} else {
		postStatus(ctx, gh, c, sha, "failure", "CLA not signed ❌")
		msg := fmt.Sprintf("@%s %s", author, c.CommentMsg)
		postComment(ctx, gh, c, pr.GetNumber(), msg)
	}
	return nil
}

func handleIssueComment(ctx context.Context, gh *github.Client, c cfg) error {
	var ev github.IssueCommentEvent
	if err := parseEvent(c.EventPath, &ev); err != nil {
		return err
	}

	// Ignore comments written by the bot itself
	author := strings.ToLower(ev.GetComment().GetUser().GetLogin())
	if _, skip := c.IgnoreAuthors[author]; skip {
		return nil
	}

	// We only care if the comment is on a PR
	if ev.GetIssue().IsPullRequest() == false {
		return nil
	}
	body := strings.ToLower(ev.GetComment().GetBody())
	if !strings.Contains(body, "@cla-bot") && !strings.Contains(body, "cla-bot check") {
		return nil
	}
	// Re-use the PR handler by synthesizing a pull_request payload
	prNum := ev.GetIssue().GetNumber()
	pr, _, err := gh.PullRequests.Get(ctx, c.RepoOwner, c.RepoName, prNum)
	if err != nil {
		return err
	}
	// Minimal PR event struct
	pre := github.PullRequestEvent{
		PullRequest: pr,
	}
	tmp, _ := json.Marshal(pre)
	tmpFile := "/tmp/pr_event.json"
	_ = os.WriteFile(tmpFile, tmp, 0o600)

	// Trick: adjust config temporarily and recurse
	subCfg := c
	subCfg.EventName = "pull_request"
	subCfg.EventPath = tmpFile
	return handlePullRequest(ctx, gh, subCfg)
}

func main() {
	c := fromEnv()
	ctx := context.Background()
	gh := newGHClient(c.Token)

	var err error
	switch c.EventName {
	case "pull_request":
		log.Info().Msg("Handling pull request")
		err = handlePullRequest(ctx, gh, c)
	case "issue_comment":
		log.Info().Msg("Handling issue comment")
		err = handleIssueComment(ctx, gh, c)
	default:
		log.
			Info().
			Str("event", c.EventName).
			Msg("Ignored event")
	}
	if err != nil {
		log.Error().Err(err).Msg("clabot error")
	}
}

// ------------------------------------------------------------
func parseEvent(path string, v interface{}) error {
	log.Info().Str("path", path).Msg("parsing event")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
