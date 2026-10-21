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
		return response.StatusCode, nil
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
	switch c.event.Action {
	case "opened", "reopened", "synchronize", "labeled", "unlabeled", "closed":
	default:
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
	if c.event.Action == "unlabeled" {
		return nil
	}
	if c.event.Action == "labeled" && c.event.Label.Name != "major" && c.event.Label.Name != "minor" && c.event.Label.Name != "patch" {
		return nil
	}
	if pr.Head.Repo.FullName != c.event.Repository.FullName || pr.Base.Ref != os.Getenv("RELEASE_BRANCH") {
		return nil
	}
	release := c.event.Action == "closed" && c.event.PullRequest.Merged
	if c.event.Action == "closed" && !release {
		return nil
	}
	if !release && (pr.State != "open" || pr.Head.SHA != c.event.PullRequest.Head.SHA) {
		return nil
	}
	if label == "" {
		if release {
			return errors.New("merged PR must have exactly one major/minor/patch label")
		}
		fmt.Println("No release label: image build skipped.")
		return nil
	}
	if release {
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
	if !release && (fresh.State != "open" || fresh.Head.SHA != sha || freshLabel == "") {
		return nil
	}
	if release && freshLabel != label {
		return errors.New("release label changed while waiting")
	}
	tag := fmt.Sprintf("pr-%d-%s", pr.Number, sha)
	digest, recorded := "", false
	if release {
		tag, digest, recorded, err = c.version(fresh, label)
		if err != nil {
			return err
		}
	}
	for _, item := range [][2]string{
		{"mode", mode}, {"sha", sha}, {"tag", tag}, {"digest", digest},
		{"image", c.registry + "/" + c.image + ":" + tag},
		{"publish", fmt.Sprint(!recorded)}, {"build", "true"},
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
	if mode == "pr" && tag != fmt.Sprintf("pr-%d-%s", c.event.PullRequest.Number, sha) {
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
	if mode == "release" {
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
	}
	pattern := regexp.MustCompile(fmt.Sprintf(`^pr-%d-[a-f0-9]{40}$`, c.event.PullRequest.Number))
	seen := map[string]bool{}
	for _, candidate := range tags {
		if !pattern.MatchString(candidate) || seen[candidate] || (mode == "pr" && candidate == tag) {
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

func run() error {
	if len(os.Args) != 2 {
		return errors.New("usage: docker-release prepare|preflight|finalize|unlock")
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
	if os.Getenv("GITHUB_EVENT_NAME") != "pull_request" || c.event.PullRequest.Number < 1 ||
		c.event.Repository.FullName != os.Getenv("GITHUB_REPOSITORY") {
		return errors.New("only pull_request events for the calling repository are supported")
	}
	switch os.Args[1] {
	case "prepare":
		return c.prepare()
	case "unlock":
		return c.unlock()
	case "finalize":
		return c.finalize()
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
