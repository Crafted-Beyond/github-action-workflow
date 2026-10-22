package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

type label struct {
	Name string `json:"name"`
}

type pullRequest struct {
	Number         int     `json:"number"`
	Title          string  `json:"title"`
	State          string  `json:"state"`
	Merged         bool    `json:"merged"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Labels         []label `json:"labels"`
	Head           struct {
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type event struct {
	Action      string      `json:"action"`
	Label       label       `json:"label"`
	PullRequest pullRequest `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue struct {
		Number      int       `json:"number"`
		PullRequest *struct{} `json:"pull_request"`
	} `json:"issue"`
	Comment struct {
		ID   int    `json:"id"`
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"comment"`
}

type gitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type gitRef struct {
	Object gitObject `json:"object"`
}

type gitTag struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Object  gitObject `json:"object"`
}

type ownership struct {
	Run   string `json:"run"`
	PR    int    `json:"pr"`
	Image string `json:"image"`
	Mode  string `json:"mode"`
}

type releaseRecord struct {
	Repository string `json:"repository"`
	Image      string `json:"image"`
	PR         int    `json:"pr"`
	SHA        string `json:"sha"`
	Digest     string `json:"digest"`
}

type previewRecord struct {
	Image     string   `json:"image"`
	Tag       string   `json:"tag"`
	SHA       string   `json:"sha"`
	Protected []string `json:"protected,omitempty"`
}

type controller struct {
	event    event
	registry string
	image    string
	adapter  string
	api      string
	token    string
	client   *http.Client
}

var (
	shaPattern     = regexp.MustCompile(`^[a-f0-9]{40}$`)
	digestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
)

func selectedLabel(labels []label) (string, error) {
	result := ""
	for _, item := range labels {
		if item.Name != "major" && item.Name != "minor" && item.Name != "patch" {
			continue
		}
		if result != "" {
			return "", errors.New("exactly one release label is allowed: major, minor or patch")
		}
		result = item.Name
	}
	return result, nil
}

func versionParts(tag string) []*big.Int {
	match := versionPattern.FindStringSubmatch(tag)
	if match == nil {
		return nil
	}
	result := make([]*big.Int, 3)
	for i := range result {
		result[i], _ = new(big.Int).SetString(match[i+1], 10)
	}
	return result
}

func compareVersions(a, b string) int {
	left, right := versionParts(a), versionParts(b)
	for i := range left {
		if comparison := left[i].Cmp(right[i]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func nextVersion(current, label string) (string, error) {
	parts := versionParts(current)
	index := map[string]int{"major": 0, "minor": 1, "patch": 2}
	position, ok := index[label]
	if parts == nil || !ok {
		return "", errors.New("invalid semantic version or release label")
	}
	parts[position].Add(parts[position], big.NewInt(1))
	for i := position + 1; i < 3; i++ {
		parts[i].SetInt64(0)
	}
	return fmt.Sprintf("%s.%s.%s", parts[0], parts[1], parts[2]), nil
}

func output(key, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return errors.New("output must not contain newlines")
	}
	file, err := os.OpenFile(os.Getenv("GITHUB_OUTPUT"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(file, "%s=%s\n", key, value)
	closeErr := file.Close()
	return errors.Join(writeErr, closeErr)
}

func (c *controller) request(method, address, authorization string, body, result any) (int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, address, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		// Do not include URLs, credentials or registry response bodies in errors.
		return 0, errors.New("API transport request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		if method == "GET" || method == "DELETE" {
			return response.StatusCode, nil
		}
		return response.StatusCode, errors.New("API mutation target not found")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return response.StatusCode, fmt.Errorf("API request failed with HTTP %d", response.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(result); err != nil {
			return response.StatusCode, fmt.Errorf("invalid API JSON response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func (c *controller) github(method, endpoint string, body, result any) (int, error) {
	return c.request(method, c.api+"/repos/"+c.event.Repository.FullName+endpoint, "Bearer "+c.token, body, result)
}

func (c *controller) pr() (pullRequest, error) {
	var result pullRequest
	status, err := c.github("GET", fmt.Sprintf("/pulls/%d", c.event.PullRequest.Number), nil, &result)
	if err == nil && status == 404 {
		err = errors.New("pull request not found")
	}
	return result, err
}

func hasLabel(pr pullRequest, name string) bool {
	for _, item := range pr.Labels {
		if item.Name == name {
			return true
		}
	}
	return false
}

func prTag(number int, sha string) string {
	return fmt.Sprintf("pr-%d-%s", number, sha[len(sha)-8:])
}

func (c *controller) note(body string) error {
	if c.event.PullRequest.Number == 0 {
		return nil
	}
	_, err := c.github("POST", fmt.Sprintf("/issues/%d/comments", c.event.PullRequest.Number),
		map[string]string{"body": body + "\n\nRun: " + os.Getenv("GITHUB_SERVER_URL") + "/" +
			c.event.Repository.FullName + "/actions/runs/" + os.Getenv("GITHUB_RUN_ID")}, nil)
	return err
}

func (c *controller) command() (string, error) {
	command := strings.TrimSpace(c.event.Comment.Body)
	switch command {
	case "/major", "/minor", "/patch", "/combine-deploy":
	default:
		return "", nil
	}
	var comment struct {
		Body string `json:"body"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	status, err := c.github("GET", fmt.Sprintf("/issues/comments/%d", c.event.Comment.ID), nil, &comment)
	if err != nil {
		return "", err
	}
	if status == 404 || comment.Body != c.event.Comment.Body || comment.User.Login != c.event.Comment.User.Login {
		return "", errors.New("command comment was removed or edited; post a new matching command")
	}
	var permission struct {
		Permission string `json:"permission"`
	}
	status, err = c.github("GET", "/collaborators/"+url.PathEscape(comment.User.Login)+"/permission", nil, &permission)
	if err != nil {
		return "", err
	}
	if status == 404 || (permission.Permission != "admin" && permission.Permission != "maintain" && permission.Permission != "write") {
		return "", errors.New("only repository writers, maintainers or administrators may trigger this command")
	}
	return command, nil
}

func (c *controller) previewName() string {
	hash := sha256.Sum256([]byte(c.registry + "/" + c.image))
	return fmt.Sprintf("ci-preview/%x/pr-%d", hash[:12], c.event.PullRequest.Number)
}

func (c *controller) preview() (*previewRecord, error) {
	ref, err := c.ref(c.previewName())
	if err != nil || ref == nil {
		return nil, err
	}
	tag, err := c.tag(ref)
	if err != nil {
		return nil, err
	}
	var record previewRecord
	if err := json.Unmarshal([]byte(tag.Message), &record); err != nil {
		return nil, errors.New("invalid preview retention record")
	}
	if record.Image != c.registry+"/"+c.image || !shaPattern.MatchString(record.SHA) ||
		(record.Tag != prTag(c.event.PullRequest.Number, record.SHA) &&
			record.Tag != fmt.Sprintf("pr-%d-%s", c.event.PullRequest.Number, record.SHA)) {
		return nil, errors.New("preview retention record does not match this image and PR")
	}
	return &record, nil
}

func (c *controller) savePreview(record previewRecord) error {
	message, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tagSHA, err := c.createTag(c.previewName(), record.SHA, string(message))
	if err != nil {
		return err
	}
	existing, err := c.ref(c.previewName())
	if err != nil {
		return err
	}
	if existing == nil {
		_, err = c.github("POST", "/git/refs", map[string]string{"ref": "refs/tags/" + c.previewName(), "sha": tagSHA}, nil)
	} else {
		_, err = c.github("PATCH", "/git/refs/tags/"+c.previewName(), map[string]any{"sha": tagSHA, "force": true}, nil)
	}
	return err
}

func (c *controller) reserveSHA(tag, sha string) error {
	hash := sha256.Sum256([]byte(c.registry + "/" + c.image))
	name := fmt.Sprintf("ci-docker-sha/%x/%s", hash[:12], tag)
	ref, err := c.ref(name)
	if err != nil {
		return err
	}
	if ref != nil {
		if ref.Object.Type != "commit" || ref.Object.SHA != sha {
			return errors.New("last-eight SHA tag collision: refusing to overwrite a different commit")
		}
		return nil
	}
	_, err = c.github("POST", "/git/refs", map[string]string{"ref": "refs/tags/" + name, "sha": sha}, nil)
	return err
}

func (c *controller) lockName() string {
	hash := sha256.Sum256([]byte(c.event.Repository.FullName))
	return fmt.Sprintf("ci-docker-lock/%x", hash[:12])
}

func (c *controller) ref(name string) (*gitRef, error) {
	var result gitRef
	status, err := c.github("GET", "/git/ref/tags/"+name, nil, &result)
	if err != nil || status == 404 {
		return nil, err
	}
	return &result, nil
}

func (c *controller) tag(ref *gitRef) (gitTag, error) {
	if ref.Object.Type != "tag" {
		return gitTag{}, errors.New("expected an annotated Git tag; manual inspection required")
	}
	var result gitTag
	status, err := c.github("GET", "/git/tags/"+ref.Object.SHA, nil, &result)
	if err == nil && status == 404 {
		err = errors.New("annotated Git tag not found")
	}
	return result, err
}

func (c *controller) owner(ref *gitRef) (ownership, error) {
	tag, err := c.tag(ref)
	if err != nil {
		return ownership{}, err
	}
	var result ownership
	if err := json.Unmarshal([]byte(tag.Message), &result); err != nil {
		return result, errors.New("invalid publication lock ownership record")
	}
	return result, nil
}

func (c *controller) createTag(name, sha, message string) (string, error) {
	var result gitTag
	_, err := c.github("POST", "/git/tags", map[string]string{
		"tag": name, "message": message, "object": sha, "type": "commit",
	}, &result)
	if err == nil && result.SHA == "" {
		err = errors.New("GitHub returned an empty annotated tag SHA")
	}
	return result.SHA, err
}

func (c *controller) acquire(sha, mode string) error {
	record := ownership{os.Getenv("GITHUB_RUN_ID"), c.event.PullRequest.Number, c.registry + "/" + c.image, mode}
	message, err := json.Marshal(record)
	if err != nil {
		return err
	}
	tagSHA, err := c.createTag(c.lockName(), sha, string(message))
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 180; attempt++ {
		status, createErr := c.github("POST", "/git/refs", map[string]string{
			"ref": "refs/tags/" + c.lockName(), "sha": tagSHA,
		}, nil)
		if createErr == nil {
			fmt.Println("Acquired image publication lock.")
			return nil
		}
		if status != 422 {
			return createErr
		}
		ref, err := c.ref(c.lockName())
		if err != nil {
			return err
		}
		if ref == nil {
			return createErr
		}
		held, err := c.owner(ref)
		if err != nil {
			return err
		}
		if held == record {
			return nil
		}
		if attempt == 0 {
			fmt.Printf("Waiting for publication run %s; failed releases must be rerun to release their lock.\n", held.Run)
		}
		time.Sleep(10 * time.Second)
	}
	return errors.New("publication lock wait exceeded 30 minutes; inspect the owning run")
}

func (c *controller) unlock() error {
	ref, err := c.ref(c.lockName())
	if err != nil || ref == nil {
		return err
	}
	owner, err := c.owner(ref)
	if err != nil {
		return err
	}
	if owner.Run != os.Getenv("GITHUB_RUN_ID") || owner.Image != c.registry+"/"+c.image {
		return nil
	}
	if owner.Mode == "release" && os.Getenv("PUBLICATION_COMPLETE") != "true" {
		fmt.Println("::warning::Release incomplete: retaining lock. Rerun the same workflow run to recover.")
		return nil
	}
	if owner.Mode == "deployment" && os.Getenv("STATE_SAFE") != "true" {
		fmt.Println("::warning::Deployment state was not saved: retaining lock. Recover the encrypted state and rerun the same run.")
		return nil
	}
	_, err = c.github("DELETE", "/git/refs/tags/"+c.lockName(), nil, nil)
	return err
}

func (c *controller) ancestor(sha, branch string) (bool, error) {
	var result struct{ Status string }
	status, err := c.github("GET", "/compare/"+url.PathEscape(sha+"..."+branch), nil, &result)
	if err != nil {
		return false, err
	}
	if status == 404 {
		return false, errors.New("Git commit comparison not found")
	}
	return result.Status == "ahead" || result.Status == "identical", nil
}

func (c *controller) version(pr pullRequest, label string) (string, string, bool, error) {
	var tags []struct {
		Name   string
		Commit struct{ SHA string }
	}
	for page := 1; ; page++ {
		var batch []struct {
			Name   string
			Commit struct{ SHA string }
		}
		status, err := c.github("GET", fmt.Sprintf("/tags?per_page=100&page=%d", page), nil, &batch)
		if err != nil {
			return "", "", false, err
		}
		if status == 404 {
			return "", "", false, errors.New("Git tags unavailable")
		}
		for _, tag := range batch {
			if versionParts(tag.Name) != nil {
				tags = append(tags, tag)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	sort.Slice(tags, func(i, j int) bool { return compareVersions(tags[i].Name, tags[j].Name) > 0 })
	for _, item := range tags {
		ref, err := c.ref(item.Name)
		if err != nil {
			return "", "", false, err
		}
		if ref == nil || ref.Object.Type != "tag" {
			continue
		}
		tag, err := c.tag(ref)
		if err != nil {
			return "", "", false, err
		}
		if !strings.HasPrefix(tag.Message, "docker-release\n") {
			continue
		}
		var record releaseRecord
		if err := json.Unmarshal([]byte(strings.TrimPrefix(tag.Message, "docker-release\n")), &record); err != nil {
			return "", "", false, errors.New("invalid recorded release metadata")
		}
		if record.Repository == c.event.Repository.FullName && record.Image == c.registry+"/"+c.image &&
			record.PR == pr.Number && record.SHA == pr.MergeCommitSHA {
			return strings.TrimPrefix(item.Name, "v"), record.Digest, true, nil
		}
	}
	var branch struct{ Commit struct{ SHA string } }
	status, err := c.github("GET", "/branches/"+url.PathEscape(os.Getenv("RELEASE_BRANCH")), nil, &branch)
	if err != nil {
		return "", "", false, err
	}
	if status == 404 {
		return "", "", false, errors.New("release branch not found")
	}
	ok, err := c.ancestor(pr.MergeCommitSHA, branch.Commit.SHA)
	if err != nil {
		return "", "", false, err
	}
	if !ok {
		return "", "", false, errors.New("merged commit is not on the release branch")
	}
	current := "0.0.0"
	for _, tag := range tags {
		ok, err := c.ancestor(tag.Commit.SHA, branch.Commit.SHA)
		if err != nil {
			return "", "", false, err
		}
		if ok {
			current = strings.TrimPrefix(tag.Name, "v")
			break
		}
	}
	next, err := nextVersion(current, label)
	return next, "", false, err
}

func (c *controller) prepare() error {
	for _, key := range []string{"build", "publish"} {
		if err := output(key, "false"); err != nil {
			return err
		}
	}
	release := os.Getenv("GITHUB_EVENT_NAME") == "pull_request" && c.event.Action == "closed" && c.event.PullRequest.Merged
	command := ""
	if os.Getenv("GITHUB_EVENT_NAME") == "issue_comment" && c.event.Action == "created" {
		var err error
		command, err = c.command()
		if err != nil {
			return err
		}
	}
	if !release && command == "" {
		return nil
	}
	pr, err := c.pr()
	if err != nil {
		return err
	}
	label, err := selectedLabel(pr.Labels)
	if err != nil {
		return err
	}
	if pr.Head.Repo.FullName != c.event.Repository.FullName || pr.Base.Ref != os.Getenv("RELEASE_BRANCH") {
		return errors.New("only same-repository PRs targeting the configured release branch are supported")
	}
	if !release && pr.State != "open" {
		return errors.New("PR commands require an open PR; deploy released versions using the manual environment workflow")
	}
	if label == "" {
		if command != "" || hasLabel(pr, "combine-deploy") {
			return errors.New("add exactly one major, minor or patch label before building or deploying")
		}
		return c.note("No release image built: this merged PR has no major, minor or patch label.")
	}
	if command == "/combine-deploy" {
		if !hasLabel(pr, "combine-deploy") {
			return errors.New("/combine-deploy requires combine-deploy plus exactly one major, minor or patch label")
		}
		if os.Getenv("ALLOW_PREVIEW") != "true" {
			return errors.New("use the combine-workflow caller for /combine-deploy; the standalone Docker workflow only builds images")
		}
	} else if !release && command != "/"+label {
		return fmt.Errorf("the command must match the release label: use /%s", label)
	}
	if release {
		if !pr.Merged || pr.MergeCommitSHA != c.event.PullRequest.MergeCommitSHA {
			return errors.New("GitHub has not confirmed this exact merged commit")
		}
		mergedLabel, err := selectedLabel(c.event.PullRequest.Labels)
		if err != nil {
			return err
		}
		if label != mergedLabel {
			return errors.New("release label must match the single label recorded at merge time")
		}
	}
	sha, mode := pr.Head.SHA, "pr"
	if release {
		sha, mode = pr.MergeCommitSHA, "release"
	}
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid source commit SHA")
	}
	if err := c.acquire(sha, mode); err != nil {
		return err
	}
	fresh, err := c.pr()
	if err != nil {
		return err
	}
	freshLabel, err := selectedLabel(fresh.Labels)
	if err != nil {
		return err
	}
	if !release && (fresh.State != "open" || fresh.Head.SHA != sha || freshLabel != label ||
		(command == "/combine-deploy" && !hasLabel(fresh, "combine-deploy"))) {
		return errors.New("PR head or labels changed while waiting; post a new matching command")
	}
	if release && freshLabel != label {
		return errors.New("release label changed while waiting")
	}
	tag := prTag(pr.Number, sha)
	digest, recorded := "", false
	if release {
		tag, digest, recorded, err = c.version(fresh, label)
		if err != nil {
			return err
		}
	} else if err := c.reserveSHA(tag, sha); err != nil {
		return err
	}
	if len(tag) > 128 {
		return errors.New("generated version exceeds the Docker image tag length limit")
	}
	for _, item := range [][2]string{
		{"mode", mode}, {"sha", sha}, {"tag", tag}, {"digest", digest},
		{"image", c.registry + "/" + c.image + ":" + tag},
		{"publish", fmt.Sprint(!recorded)}, {"build", "true"},
		{"preview", fmt.Sprint(command == "/combine-deploy")},
		{"pr", fmt.Sprint(pr.Number)},
	} {
		if err := output(item[0], item[1]); err != nil {
			return err
		}
	}
	return nil
}

type registryClient struct {
	controller *controller
	token      string
	script     string
}

func (c *controller) registryClient() (*registryClient, error) {
	result := &registryClient{controller: c}
	if c.adapter == "custom" {
		root, err := filepath.EvalSymlinks(filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "registry-control"))
		if err != nil {
			return nil, err
		}
		script, err := filepath.EvalSymlinks(filepath.Join(root, os.Getenv("REGISTRY_CLEANUP_SCRIPT")))
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(root, script)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Ext(script) != ".go" {
			return nil, errors.New("custom cleanup adapter must be a Go source file inside the trusted checkout")
		}
		result.script = script
		return result, nil
	}
	var login struct{ Token string }
	status, err := c.request("POST", "https://hub.docker.com/v2/users/login/", "", map[string]string{
		"username": os.Getenv("REGISTRY_USERNAME"), "password": os.Getenv("REGISTRY_PASSWORD"),
	}, &login)
	if err != nil {
		return nil, err
	}
	if status == 404 || login.Token == "" {
		return nil, errors.New("Docker Hub authentication did not return a token")
	}
	result.token = login.Token
	return result, nil
}

func (r *registryClient) custom(operation, tag string, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "run", r.script, operation)
	command.Dir = filepath.Dir(r.script)
	command.Env = append(os.Environ(), "CLEANUP_TAG="+tag)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return fmt.Errorf("custom Go registry adapter %s failed; output withheld to protect credentials", operation)
	}
	if result != nil {
		if err := json.Unmarshal(out, result); err != nil {
			return errors.New("custom registry adapter must return a JSON array of tag names")
		}
	}
	return nil
}

func (r *registryClient) list() ([]string, error) {
	if r.script != "" {
		var tags []string
		if err := r.custom("list", "", &tags); err != nil {
			return nil, err
		}
		if tags == nil {
			return nil, errors.New("custom registry adapter returned null, expected a tag array")
		}
		return tags, nil
	}
	base := "https://hub.docker.com/v2/repositories/" + r.controller.image + "/tags/"
	address := base + "?page_size=100"
	var tags []string
	for address != "" {
		parsed, err := url.Parse(address)
		expected, _ := url.Parse(base)
		if err != nil || parsed.Scheme != "https" || parsed.Host != expected.Host || parsed.Path != expected.Path || parsed.User != nil {
			return nil, errors.New("unexpected Docker Hub pagination URL")
		}
		var page struct {
			Next    string                  `json:"next"`
			Results []struct{ Name string } `json:"results"`
		}
		status, err := r.controller.request("GET", address, "JWT "+r.token, nil, &page)
		if err != nil {
			return nil, err
		}
		if status == 404 {
			return nil, errors.New("Docker Hub repository must already exist and be accessible")
		}
		if page.Results == nil {
			return nil, errors.New("invalid Docker Hub tag-list response")
		}
		for _, tag := range page.Results {
			tags = append(tags, tag.Name)
		}
		address = page.Next
	}
	return tags, nil
}

func (r *registryClient) delete(tag string) error {
	if r.script != "" {
		return r.custom("delete", tag, nil)
	}
	_, err := r.controller.request("DELETE", "https://hub.docker.com/v2/repositories/"+r.controller.image+"/tags/"+url.PathEscape(tag)+"/", "JWT "+r.token, nil, nil)
	return err
}

func contains(tags []string, wanted string) bool {
	for _, tag := range tags {
		if tag == wanted {
			return true
		}
	}
	return false
}

func (c *controller) finalize() error {
	mode, sha, tag, digest := os.Getenv("BUILD_MODE"), os.Getenv("BUILD_SHA"), os.Getenv("BUILD_TAG"), os.Getenv("BUILD_DIGEST")
	if (mode != "pr" && mode != "release") || !shaPattern.MatchString(sha) || !digestPattern.MatchString(digest) {
		return errors.New("invalid publication metadata")
	}
	if mode == "pr" && tag != prTag(c.event.PullRequest.Number, sha) {
		return errors.New("invalid PR image tag")
	}
	if mode == "release" && !versionPattern.MatchString(tag) {
		return errors.New("invalid release image tag")
	}
	ref, err := c.ref(c.lockName())
	if err != nil {
		return err
	}
	if ref == nil {
		return errors.New("publication lock disappeared; refusing cleanup")
	}
	owner, err := c.owner(ref)
	if err != nil {
		return err
	}
	if owner != (ownership{os.Getenv("GITHUB_RUN_ID"), c.event.PullRequest.Number, c.registry + "/" + c.image, mode}) {
		return errors.New("publication lock belongs to another publisher")
	}
	registry, err := c.registryClient()
	if err != nil {
		return err
	}
	tags, err := registry.list()
	if err != nil {
		return err
	}
	if !contains(tags, tag) {
		return errors.New("replacement image tag is not visible; old PR tags will not be deleted")
	}
	if mode == "pr" {
		pr, err := c.pr()
		if err != nil {
			return err
		}
		if pr.State != "open" || pr.Head.SHA != sha {
			return errors.New("PR changed during the image build; keeping existing images and refusing deployment, post a new command")
		}
		label, err := selectedLabel(pr.Labels)
		if err != nil {
			return err
		}
		command := strings.TrimSpace(c.event.Comment.Body)
		if label == "" || (command != "/combine-deploy" && command != "/"+label) ||
			(command == "/combine-deploy" && !hasLabel(pr, "combine-deploy")) {
			return errors.New("PR labels changed during the build; restore valid labels and post a new matching command")
		}
	}
	if mode == "release" {
		pr, err := c.pr()
		if err != nil {
			return err
		}
		currentLabel, err := selectedLabel(pr.Labels)
		if err != nil {
			return err
		}
		mergedLabel, err := selectedLabel(c.event.PullRequest.Labels)
		if err != nil {
			return err
		}
		if !pr.Merged || pr.MergeCommitSHA != sha || currentLabel == "" || currentLabel != mergedLabel {
			return errors.New("merged status, commit or release label changed; refusing to record a release tag")
		}
		record := releaseRecord{c.event.Repository.FullName, c.registry + "/" + c.image, c.event.PullRequest.Number, sha, digest}
		ref, err := c.ref("v" + tag)
		if err != nil {
			return err
		}
		if ref != nil {
			existing, err := c.tag(ref)
			if err != nil {
				return err
			}
			var prior releaseRecord
			if !strings.HasPrefix(existing.Message, "docker-release\n") {
				return errors.New("version tag is not owned by this release")
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(existing.Message, "docker-release\n")), &prior); err != nil {
				return err
			}
			if prior != record || existing.Object.SHA != sha {
				return errors.New("version tag belongs to a different publication or image digest")
			}
		} else {
			data, err := json.Marshal(record)
			if err != nil {
				return err
			}
			tagSHA, err := c.createTag("v"+tag, sha, "docker-release\n"+string(data))
			if err != nil {
				return err
			}
			if _, err := c.github("POST", "/git/refs", map[string]string{"ref": "refs/tags/v" + tag, "sha": tagSHA}, nil); err != nil {
				return err
			}
		}
		fmt.Printf("Recorded v%s on merged release-branch commit %s.\n", tag, sha)
		pr, err = c.pr()
		if err != nil {
			return err
		}
		if !pr.Merged || pr.MergeCommitSHA != sha {
			return errors.New("PR is not merged at the published release commit")
		}
		title := regexp.MustCompile(` \[image: v?[0-9]+\.[0-9]+\.[0-9]+\]$`).ReplaceAllString(pr.Title, "")
		suffix := " [image: " + tag + "]"
		if len(suffix) > 256 {
			return errors.New("release version is too long for a PR title")
		}
		runes := []rune(title)
		if len(runes)+len(suffix) > 256 {
			title = string(runes[:256-len(suffix)])
		}
		if _, err := c.github("PATCH", fmt.Sprintf("/pulls/%d", pr.Number),
			map[string]string{"title": title + suffix}, nil); err != nil {
			return err
		}
	}
	protected, err := c.preview()
	if err != nil {
		return err
	}
	if mode == "pr" && os.Getenv("BUILD_PREVIEW") == "true" {
		fmt.Println("Deferring PR cleanup until a preview is healthy; retained previews must keep their image.")
		return c.note("Published `" + c.registry + "/" + c.image + ":" + tag + "`. Preview images are retained until deployment succeeds.")
	}
	keep := ""
	if mode == "pr" {
		keep = tag
	}
	if err := c.cleanupTags(registry, tags, keep, protected); err != nil {
		return err
	}
	return c.note("Published `" + c.registry + "/" + c.image + ":" + tag + "`. No actual-environment deployment was triggered.")
}

func (c *controller) cleanupTags(registry *registryClient, tags []string, keep string, protected *previewRecord) error {
	pattern := regexp.MustCompile(fmt.Sprintf(`^pr-%d-([a-f0-9]{8}|[a-f0-9]{40})$`, c.event.PullRequest.Number))
	seen := map[string]bool{}
	for _, candidate := range tags {
		if !pattern.MatchString(candidate) || seen[candidate] || candidate == keep ||
			(protected != nil && (candidate == protected.Tag || contains(protected.Protected, candidate))) {
			continue
		}
		seen[candidate] = true
		deleted := false
		for attempt := 0; attempt < 3; attempt++ {
			deleteErr := registry.delete(candidate)
			if deleteErr == nil {
				remaining, listErr := registry.list()
				deleted = listErr == nil && !contains(remaining, candidate)
			}
			if deleted {
				break
			}
			fmt.Printf("::warning::Temporary tag %s: deletion/verification attempt %d failed.\n", candidate, attempt+1)
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 2 * time.Second)
			}
		}
		if !deleted {
			return fmt.Errorf("failed to delete temporary tag %s; rerun this workflow to retry cleanup", candidate)
		}
		fmt.Printf("Deleted temporary tag %s.\n", candidate)
	}
	return nil
}

func (c *controller) deploymentVersion(requested string) (string, string, error) {
	var branch struct{ Commit struct{ SHA string } }
	status, err := c.github("GET", "/branches/"+url.PathEscape(os.Getenv("RELEASE_BRANCH")), nil, &branch)
	if err != nil {
		return "", "", err
	}
	if status == 404 || !shaPattern.MatchString(branch.Commit.SHA) {
		return "", "", errors.New("release branch not found")
	}
	type candidate struct {
		Name   string
		Commit struct{ SHA string }
	}
	var tags []candidate
	for page := 1; ; page++ {
		var batch []candidate
		status, err := c.github("GET", fmt.Sprintf("/tags?per_page=100&page=%d", page), nil, &batch)
		if err != nil {
			return "", "", err
		}
		if status == 404 {
			return "", "", errors.New("application Git tags unavailable")
		}
		for _, tag := range batch {
			if versionParts(tag.Name) != nil && (requested == "" || strings.TrimPrefix(tag.Name, "v") == requested) {
				tags = append(tags, tag)
			}
		}
		if len(batch) < 100 {
			break
		}
	}
	sort.Slice(tags, func(i, j int) bool { return compareVersions(tags[i].Name, tags[j].Name) > 0 })
	for _, tag := range tags {
		ok, err := c.ancestor(tag.Commit.SHA, branch.Commit.SHA)
		if err != nil {
			return "", "", err
		}
		if ok {
			return strings.TrimPrefix(tag.Name, "v"), tag.Commit.SHA, nil
		}
	}
	return "", "", errors.New("no matching stable release tag on the release branch; publish a release first")
}

func deploymentIdentity(repository, suffix string) string {
	name := strings.ToLower(strings.Split(repository, "/")[1])
	name = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(name, "-")
	name = strings.Trim(name, "-")
	if len(name)+len(suffix) > 48 {
		hash := sha256.Sum256([]byte(repository))
		name = strings.TrimRight(name[:48-len(suffix)-9], "-") + fmt.Sprintf("-%x", hash[:4])
	}
	return name + suffix
}

func (c *controller) deploymentPrepare() error {
	if len(os.Getenv("TF_VAR_state_encryption_passphrase")) < 32 || os.Getenv("STATE_REPO_TOKEN") == "" {
		return errors.New("deployment requires a state_repo_token and a stable state_encryption_passphrase of at least 32 characters")
	}
	stateRepository := os.Getenv("STATE_REPOSITORY")
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(stateRepository) {
		return errors.New("state_repository must be owner/repository")
	}
	var stateRepo struct {
		Private bool `json:"private"`
	}
	status, err := c.request("GET", c.api+"/repos/"+stateRepository, "Bearer "+os.Getenv("STATE_REPO_TOKEN"), nil, &stateRepo)
	if err != nil {
		return err
	}
	if status == 404 || !stateRepo.Private {
		return errors.New("state_repository must exist, be private and be accessible to state_repo_token")
	}
	if (os.Getenv("TF_VAR_chart_username") == "") != (os.Getenv("TF_VAR_chart_password") == "") {
		return errors.New("provide both chart_username and chart_password for a private HTTPS chart repository")
	}
	if (os.Getenv("IMAGE_PULL_USERNAME") == "") != (os.Getenv("IMAGE_PULL_PASSWORD") == "") {
		return errors.New("provide both image_pull_username and image_pull_password, or omit both")
	}
	preview := os.Getenv("GITHUB_EVENT_NAME") == "issue_comment"
	tag, sha := os.Getenv("BUILD_TAG"), os.Getenv("BUILD_SHA")
	environment := os.Getenv("DEPLOY_ENVIRONMENT")
	suffix := ""
	if preview {
		if os.Getenv("BUILD_PREVIEW") != "true" || !shaPattern.MatchString(sha) || tag != prTag(c.event.PullRequest.Number, sha) {
			return errors.New("preview requires a successfully published /combine-deploy image")
		}
		environment = fmt.Sprintf("pr%d", c.event.PullRequest.Number)
		suffix = "-" + environment
	} else {
		if os.Getenv("GITHUB_EVENT_NAME") != "workflow_dispatch" ||
			os.Getenv("GITHUB_REF") != "refs/heads/"+os.Getenv("RELEASE_BRANCH") {
			return errors.New("actual-environment deployments require a manual run from the release branch")
		}
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`).MatchString(environment) || environment == "preview" ||
			regexp.MustCompile(`^pr[0-9]+$`).MatchString(environment) {
			return errors.New("supply a real environment such as dev or prod, not a reserved preview name")
		}
		requested := strings.TrimPrefix(strings.TrimSpace(os.Getenv("DEPLOY_TAG")), "v")
		if requested != "" && !versionPattern.MatchString(requested) {
			return errors.New("deployment tag must be a stable semantic version, for example 1.2.3")
		}
		var err error
		tag, sha, err = c.deploymentVersion(requested)
		if err != nil {
			return err
		}
	}
	name := deploymentIdentity(c.event.Repository.FullName, suffix)
	if !preview && os.Getenv("DEPLOY_APPLICATION_NAME") != "" {
		name = os.Getenv("DEPLOY_APPLICATION_NAME")
	}
	if len(name) > 48 || !regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`).MatchString(name) {
		return errors.New("deployment name must be DNS-safe and at most 48 characters")
	}
	domain := strings.TrimSpace(os.Getenv("DEPLOY_DOMAIN"))
	hostname := name + "." + domain
	if len(hostname) > 253 || !regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(hostname) {
		return errors.New("deployment domain must be a valid lowercase DNS domain without scheme or path")
	}
	var values map[string]any
	if err := json.Unmarshal([]byte(os.Getenv("HELM_VALUES_JSON")), &values); err != nil || values == nil {
		return errors.New("helm_values_json must be a JSON object")
	}
	route, ok := values["httpRoute"].(map[string]any)
	if !ok {
		return errors.New("helm_values_json must include httpRoute.parentRefs for your existing Gateway")
	}
	parents, ok := route["parentRefs"].([]any)
	if !ok || len(parents) == 0 {
		return errors.New("configure at least one httpRoute.parentRefs entry for your existing Gateway")
	}
	for _, parent := range parents {
		fields, ok := parent.(map[string]any)
		if !ok {
			return errors.New("HTTPRoute parentRefs must be objects")
		}
		parentName, ok := fields["name"].(string)
		if !ok || parentName == "" {
			return errors.New("each HTTPRoute parentRef requires a Gateway name")
		}
	}
	// Fixed names in these fields bypass the chart's release-specific isolation.
	if preview {
		for _, key := range []string{"serviceAccount", "extraServices", "httpRoute"} {
			if item, ok := values[key].(map[string]any); ok {
				if key == "serviceAccount" && item["name"] != nil && item["name"] != "" {
					return errors.New("preview serviceAccount.name must be empty so the chart uses its release name")
				}
				if key == "extraServices" && len(item) > 0 {
					return errors.New("preview extraServices must be empty; use release-derived application Services")
				}
				if key == "httpRoute" {
					if rules, ok := item["rules"].([]any); ok {
						for _, entry := range rules {
							rule, ok := entry.(map[string]any)
							if !ok {
								return errors.New("HTTPRoute rules must be objects")
							}
							if _, exists := rule["backendRefs"]; exists {
								return errors.New("preview HTTPRoute rules must use the chart's default release-specific backend")
							}
						}
					}
				}
			}
		}
	}
	if strings.TrimSpace(os.Getenv("CHART_REPOSITORY_URL")) == "" || strings.TrimSpace(os.Getenv("CHART_REVISION")) == "" ||
		strings.TrimSpace(os.Getenv("CHART_PATH")) == "" {
		return errors.New("chart repository URL, revision and path are required")
	}
	chartURL, err := url.Parse(os.Getenv("CHART_REPOSITORY_URL"))
	if err != nil || chartURL.Scheme != "https" || chartURL.Host == "" || chartURL.User != nil || chartURL.RawQuery != "" || chartURL.Fragment != "" {
		return errors.New("chart_repository_url must be an HTTPS Git URL without embedded credentials")
	}
	registry, err := c.registryClient()
	if err != nil {
		return err
	}
	tags, err := registry.list()
	if err != nil {
		return err
	}
	if !contains(tags, tag) {
		return errors.New("selected image tag is not published in the configured registry")
	}
	config := map[string]any{
		"name": name, "repository_name": strings.Split(c.event.Repository.FullName, "/")[1],
		"environment": environment, "hostname": hostname, "registry": c.registry,
		"image_repository": c.registry + "/" + c.image, "image_tag": tag,
		"chart_source": map[string]string{
			"repo_url": os.Getenv("CHART_REPOSITORY_URL"), "target_revision": os.Getenv("CHART_REVISION"), "path": os.Getenv("CHART_PATH"),
		},
		"helm_values": values, "argocd_namespace": os.Getenv("ARGOCD_NAMESPACE"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "combine.auto.tfvars.json"), data, 0600); err != nil {
		return err
	}
	for _, item := range [][2]string{
		{"name", name}, {"hostname", hostname}, {"tag", tag}, {"sha", sha},
		{"preview", fmt.Sprint(preview)}, {"environment", environment},
		{"state_path", "combine/" + c.event.Repository.FullName + "/" + environment},
	} {
		if err := output(item[0], item[1]); err != nil {
			return err
		}
	}
	return nil
}

func (c *controller) deploymentLock() error {
	sha := os.Getenv("BUILD_SHA")
	if !shaPattern.MatchString(sha) {
		return errors.New("invalid deployment commit")
	}
	if err := c.acquire(sha, "deployment"); err != nil {
		return err
	}
	if os.Getenv("BUILD_PREVIEW") == "true" {
		pr, err := c.pr()
		if err != nil {
			return err
		}
		label, err := selectedLabel(pr.Labels)
		if err != nil {
			return err
		}
		if pr.State != "open" || pr.Head.SHA != sha || label == "" || !hasLabel(pr, "combine-deploy") ||
			pr.Head.Repo.FullName != c.event.Repository.FullName || pr.Base.Ref != os.Getenv("RELEASE_BRANCH") {
			return errors.New("preview PR head, state or labels changed; post a new /combine-deploy command")
		}
	}
	registry, err := c.registryClient()
	if err != nil {
		return err
	}
	tags, err := registry.list()
	if err != nil {
		return err
	}
	if !contains(tags, os.Getenv("BUILD_TAG")) {
		return errors.New("selected image disappeared while waiting for the deployment lock")
	}
	if os.Getenv("BUILD_PREVIEW") == "true" {
		prior, err := c.preview()
		if err != nil {
			return err
		}
		record := previewRecord{Image: c.registry + "/" + c.image, Tag: os.Getenv("BUILD_TAG"), SHA: sha}
		if prior != nil {
			record.Protected = append(prior.Protected, prior.Tag)
		}
		if err := c.savePreview(record); err != nil {
			return err
		}
	}
	return nil
}

func kubectl(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "kubectl", args...).Output()
	if err != nil {
		return nil, errors.New("kubectl request failed; check cluster connectivity and RBAC")
	}
	return out, nil
}

func (c *controller) deploymentComplete() error {
	ref, err := c.ref(c.lockName())
	if err != nil {
		return err
	}
	if ref == nil {
		return errors.New("deployment lock disappeared")
	}
	owner, err := c.owner(ref)
	if err != nil {
		return err
	}
	if owner != (ownership{os.Getenv("GITHUB_RUN_ID"), c.event.PullRequest.Number, c.registry + "/" + c.image, "deployment"}) {
		return errors.New("deployment lock belongs to another run")
	}
	namespace, name := os.Getenv("ARGOCD_NAMESPACE"), os.Getenv("DEPLOY_NAME")
	start := time.Now().UTC().Truncate(time.Second)
	if _, err := kubectl("-n", namespace, "annotate", "application", name, "argocd.argoproj.io/refresh=hard", "--overwrite"); err != nil {
		return err
	}
	healthy := false
	for attempt := 0; attempt < 120; attempt++ {
		data, err := kubectl("-n", namespace, "get", "application", name, "-o", "json")
		if err != nil {
			return err
		}
		var app struct {
			Spec   struct{ Source map[string]any } `json:"spec"`
			Status struct {
				ReconciledAt time.Time               `json:"reconciledAt"`
				Health       struct{ Status string } `json:"health"`
				Sync         struct {
					Status     string                          `json:"status"`
					Revision   string                          `json:"revision"`
					ComparedTo struct{ Source map[string]any } `json:"comparedTo"`
				} `json:"sync"`
				OperationState struct {
					Phase      string `json:"phase"`
					SyncResult struct {
						Source   map[string]any `json:"source"`
						Revision string         `json:"revision"`
					} `json:"syncResult"`
				} `json:"operationState"`
			} `json:"status"`
		}
		if err := json.Unmarshal(data, &app); err != nil {
			return errors.New("invalid Argo CD Application response")
		}
		if !app.Status.ReconciledAt.Before(start) && app.Status.Sync.Status == "Synced" &&
			app.Status.Health.Status == "Healthy" && reflect.DeepEqual(app.Spec.Source, app.Status.Sync.ComparedTo.Source) &&
			app.Status.OperationState.Phase == "Succeeded" &&
			reflect.DeepEqual(app.Spec.Source, app.Status.OperationState.SyncResult.Source) &&
			app.Status.Sync.Revision == app.Status.OperationState.SyncResult.Revision {
			ready, err := routeReady(name)
			if err != nil {
				return err
			}
			if ready {
				healthy = true
				break
			}
		}
		time.Sleep(10 * time.Second)
	}
	if !healthy {
		return errors.New("Argo CD and HTTPRoute did not become synced, healthy and accepted within 20 minutes")
	}
	if os.Getenv("BUILD_PREVIEW") == "true" {
		sha, tag := os.Getenv("BUILD_SHA"), os.Getenv("BUILD_TAG")
		if !shaPattern.MatchString(sha) || tag != prTag(c.event.PullRequest.Number, sha) {
			return errors.New("invalid healthy preview image metadata")
		}
		record := previewRecord{Image: c.registry + "/" + c.image, Tag: tag, SHA: sha}
		if err := c.savePreview(record); err != nil {
			return err
		}
		registry, err := c.registryClient()
		if err != nil {
			return err
		}
		tags, err := registry.list()
		if err != nil {
			return err
		}
		if !contains(tags, tag) {
			return errors.New("healthy preview image is no longer visible; refusing cleanup")
		}
		if err := c.cleanupTags(registry, tags, tag, &record); err != nil {
			return err
		}
	}
	return output("healthy", "true")
}

func routeReady(name string) (bool, error) {
	data, err := kubectl("-n", name, "get", "httproute", name, "-o", "json")
	if err != nil {
		return false, err
	}
	var route struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			Parents []struct {
				Conditions []struct {
					Type               string `json:"type"`
					Status             string `json:"status"`
					ObservedGeneration int64  `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"parents"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &route); err != nil {
		return false, errors.New("invalid HTTPRoute response")
	}
	if len(route.Status.Parents) == 0 {
		return false, nil
	}
	for _, parent := range route.Status.Parents {
		accepted, resolved := false, false
		for _, condition := range parent.Conditions {
			if condition.Status != "True" || condition.ObservedGeneration != route.Metadata.Generation {
				continue
			}
			if condition.Type == "Accepted" {
				accepted = true
			}
			if condition.Type == "ResolvedRefs" {
				resolved = true
			}
		}
		if !accepted || !resolved {
			return false, nil
		}
	}
	return true, nil
}

func deploymentCleanup() error {
	if os.Getenv("GITHUB_WORKSPACE") == "" || os.Getenv("RUNNER_TEMP") == "" {
		return errors.New("runner workspace and temporary directory are required for cleanup")
	}
	paths := []string{
		filepath.Join(os.Getenv("RUNNER_TEMP"), "combine-kubeconfig"),
		filepath.Join(os.Getenv("RUNNER_TEMP"), "combine.auto.tfvars.json"),
		filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", "combine.auto.tfvars.json"),
		filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", "tfplan"),
		filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", "crash.log"),
	}
	preserve := os.Getenv("PRESERVE_DEPLOYMENT_STATE") == "true"
	if !preserve {
		paths = append(paths,
			filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", "terraform.tfstate"),
			filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", "terraform.tfstate.backup"))
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("could not remove secret-bearing deployment file: %w", err)
		}
	}
	for _, path := range []string{
		filepath.Join(os.Getenv("GITHUB_WORKSPACE"), ".combine-state"),
		filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "infrastructure", "deployment", ".terraform"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("could not clean deployment working directory: %w", err)
		}
	}
	if preserve {
		return errors.New("state could not be pushed or uploaded; encrypted terraform.tfstate and backup remain in infrastructure/deployment for manual recovery")
	}
	return nil
}

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: docker-release prepare|preflight|finalize|unlock|deployment-prepare|deployment-lock|deployment-complete|report")
	}
	if os.Args[1] == "deployment-cleanup" {
		return deploymentCleanup()
	}
	c := &controller{
		registry: os.Getenv("IMAGE_REGISTRY"), image: os.Getenv("IMAGE_REPOSITORY"),
		adapter: os.Getenv("REGISTRY_ADAPTER"), token: os.Getenv("GH_TOKEN"),
		api: strings.TrimSuffix(os.Getenv("GITHUB_API_URL"), "/"),
		client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("API redirects are not allowed")
		}},
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?$`).MatchString(c.registry) ||
		!regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)+$`).MatchString(c.image) {
		return errors.New("invalid registry hostname or lowercase namespace/repository image path")
	}
	switch c.adapter {
	case "dockerhub":
		if c.registry != "docker.io" || len(strings.Split(c.image, "/")) != 2 {
			return errors.New("Docker Hub requires docker.io and namespace/repository; other registries need a custom adapter")
		}
	case "custom":
		if os.Getenv("REGISTRY_CLEANUP_SCRIPT") == "" {
			return errors.New("custom registry requires a trusted Go registry_cleanup_script")
		}
	default:
		return errors.New("registry_adapter must be dockerhub or custom")
	}
	if os.Getenv("REGISTRY_USERNAME") == "" || os.Getenv("REGISTRY_PASSWORD") == "" || c.token == "" {
		return errors.New("GitHub and registry credentials are required")
	}
	data, err := os.ReadFile(os.Getenv("GITHUB_EVENT_PATH"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &c.event); err != nil {
		return err
	}
	if c.event.Repository.FullName != os.Getenv("GITHUB_REPOSITORY") {
		return errors.New("event repository must match the calling repository")
	}
	switch os.Getenv("GITHUB_EVENT_NAME") {
	case "issue_comment":
		if c.event.Issue.PullRequest == nil || c.event.Issue.Number < 1 {
			return nil
		}
		c.event.PullRequest.Number = c.event.Issue.Number
	case "pull_request":
		if c.event.PullRequest.Number < 1 {
			return errors.New("missing PR number")
		}
	case "workflow_dispatch":
	default:
		return errors.New("unsupported workflow event")
	}
	switch os.Args[1] {
	case "prepare":
		err := c.prepare()
		if err != nil {
			return errors.Join(err, c.note("Build/deployment request rejected: "+err.Error()))
		}
		return nil
	case "unlock":
		return c.unlock()
	case "finalize":
		return c.finalize()
	case "deployment-prepare":
		return c.deploymentPrepare()
	case "deployment-lock":
		return c.deploymentLock()
	case "deployment-complete":
		return c.deploymentComplete()
	case "report":
		body := os.Getenv("REPORT_BODY")
		summary, err := os.OpenFile(os.Getenv("GITHUB_STEP_SUMMARY"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, writeErr := fmt.Fprintln(summary, body)
		closeErr := summary.Close()
		return errors.Join(writeErr, closeErr, c.note(body))
	case "preflight":
		registry, err := c.registryClient()
		if err != nil {
			return err
		}
		if _, err := registry.list(); err != nil {
			return err
		}
		fmt.Println("Registry tag-list access verified; deletion permissions must also be granted.")
		return nil
	default:
		return errors.New("unknown Docker controller operation")
	}
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Docker release failed: %v\n", err)
		os.Exit(1)
	}
}
