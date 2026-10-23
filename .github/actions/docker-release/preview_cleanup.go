package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (c *controller) validateCleanupPR() error {
	if os.Getenv("GITHUB_EVENT_NAME") != "pull_request" || c.event.Action != "closed" ||
		c.event.PullRequest.Number < 1 {
		return errors.New("preview cleanup requires a closed pull request")
	}
	pr, err := c.pr()
	if err != nil {
		return err
	}
	if pr.State != "closed" || pr.Head.Repo.FullName != c.event.Repository.FullName ||
		pr.Base.Ref != os.Getenv("RELEASE_BRANCH") || pr.Head.SHA != c.event.PullRequest.Head.SHA ||
		!shaPattern.MatchString(pr.Head.SHA) {
		return errors.New("cleanup requires an unchanged, closed, same-repository PR targeting the release branch")
	}
	return nil
}

func (c *controller) previewCleanupPrepare() error {
	if err := c.validateCleanupPR(); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(os.Getenv("STATE_REPOSITORY")) {
		return errors.New("state_repository must be owner/repository")
	}
	var repository struct {
		Private bool `json:"private"`
	}
	status, err := c.request("GET", c.api+"/repos/"+os.Getenv("STATE_REPOSITORY"), "Bearer "+c.token, nil, &repository)
	if err != nil {
		return err
	}
	if status == 404 || !repository.Private {
		return errors.New("cleanup state repository must be private and accessible")
	}
	directory, err := infrastructureDirectory()
	if err != nil {
		return err
	}
	environment := fmt.Sprintf("pr%d", c.event.PullRequest.Number)
	config := map[string]any{"combine_runtime": map[string]any{
		"repository": c.event.Repository.FullName, "environment": environment,
		"pr_number": c.event.PullRequest.Number, "preview": true, "sha": c.event.PullRequest.Head.SHA,
		"registry": c.registry, "image_repository": c.registry + "/" + c.image, "image_tag": "cleanup",
	}}
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("RUNNER_TEMP"), "combine.auto.tfvars.json"), data, 0600); err != nil {
		return err
	}
	for _, item := range [][2]string{
		{"working_directory", directory},
		{"state_path", "combine/" + c.event.Repository.FullName + "/" + environment},
		{"name", strings.Split(c.event.Repository.FullName, "/")[1] + "-" + environment},
	} {
		if err := output(item[0], item[1]); err != nil {
			return err
		}
	}
	return nil
}

func verifyPreviewState(data []byte, name string) error {
	var state struct {
		Outputs struct {
			Deployment struct {
				Value struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"value"`
			} `json:"combine_deployment"`
		} `json:"outputs"`
		Resources []struct {
			Mode      string `json:"mode"`
			Type      string `json:"type"`
			Name      string `json:"name"`
			Instances []struct {
				Attributes struct {
					Metadata []struct {
						Name   string            `json:"name"`
						Labels map[string]string `json:"labels"`
					} `json:"metadata"`
				} `json:"attributes"`
			} `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return errors.New("invalid saved preview state")
	}
	managed := false
	ownedNamespace := false
	for _, resource := range state.Resources {
		if resource.Mode != "managed" || len(resource.Instances) == 0 {
			continue
		}
		managed = true
		if resource.Type != "kubernetes_namespace_v1" {
			continue
		}
		for _, instance := range resource.Instances {
			metadata := instance.Attributes.Metadata
			if resource.Name != "preview" || len(metadata) != 1 || metadata[0].Name != name ||
				metadata[0].Labels["app.kubernetes.io/managed-by"] != "combine-workflow" ||
				metadata[0].Labels["craftedbeyond.com/environment"] != name[strings.LastIndex(name, "-")+1:] {
				return errors.New("saved state contains a namespace not owned by this preview")
			}
			ownedNamespace = true
		}
	}
	if !managed {
		return nil
	}
	deployment := state.Outputs.Deployment.Value
	if !ownedNamespace || deployment.Name != name || deployment.Namespace != name {
		return errors.New("saved state is not a complete owned preview; recover/import it before cleanup")
	}
	return nil
}

func (c *controller) previewCleanupVerify() error {
	if err := c.validateCleanupPR(); err != nil {
		return err
	}
	directory, err := infrastructureDirectory()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(os.Getenv("GITHUB_WORKSPACE"), directory, "terraform.tfstate"))
	if err != nil {
		return fmt.Errorf("cleanup requires saved preview state: %w", err)
	}
	name := fmt.Sprintf("%s-pr%d", strings.Split(c.event.Repository.FullName, "/")[1], c.event.PullRequest.Number)
	return verifyPreviewState(data, name)
}
