/*
Copyright 2023 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/go-github/v53/github"
	"github.com/palantir/go-githubapp/githubapp"
	"github.com/pkg/errors"
)

const (
	backportLabelPrefix    = "Backport to: "
	forwardportLabelPrefix = "Forwardport to: "

	backport    = "backport"
	forwardport = "forwardport"
)

var (
	alwaysAddLabels = []string{
		"NeedsWebsiteDocsUpdate",
		"NeedsDescriptionUpdate",
		"NeedsIssue",
	}
)

type PullRequestHandler struct {
	githubapp.ClientCreator

	reviewChecklist string
}

type prInformation struct {
	repo      *github.Repository
	num       int
	repoOwner string
	repoName  string
	merged    bool
}

func getPRInformation(event github.PullRequestEvent) prInformation {
	repo := event.GetRepo()
	merged := false
	pr := event.GetPullRequest()
	if pr != nil {
		merged = pr.GetMerged()
	}
	return prInformation{
		repo:      repo,
		num:       event.GetNumber(),
		repoOwner: repo.GetOwner().GetLogin(),
		repoName:  repo.GetName(),
		merged:    merged,
	}
}

func (h *PullRequestHandler) Handles() []string {
	return []string{"pull_request"}
}

func (h *PullRequestHandler) Handle(ctx context.Context, eventType, deliveryID string, payload []byte) error {
	var event github.PullRequestEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return errors.Wrap(err, "failed to parse issue comment event payload")
	}

	switch event.GetAction() {
	case "opened":
		prInfo := getPRInformation(event)
		err := h.addReviewChecklist(ctx, event, prInfo)
		if err != nil {
			return err
		}
		err = h.addLabels(ctx, event, prInfo)
		if err != nil {
			return err
		}
		err = h.createErrorDocumentation(ctx, event, prInfo)
		if err != nil {
			return err
		}
	case "closed":
		prInfo := getPRInformation(event)
		if prInfo.merged {
			err := h.backportPR(ctx, event, prInfo)
			if err != nil {
				return err
			}
		}
	case "synchronize":
		prInfo := getPRInformation(event)
		err := h.createErrorDocumentation(ctx, event, prInfo)
		if err != nil {
			return err
		}
	}
	return nil
}

func (h *PullRequestHandler) addReviewChecklist(ctx context.Context, event github.PullRequestEvent, prInfo prInformation) error {
	installationID := githubapp.GetInstallationIDFromEvent(&event)

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	ctx, logger := githubapp.PreparePRContext(ctx, installationID, prInfo.repo, event.GetNumber())

	prComment := github.IssueComment{
		Body: &h.reviewChecklist,
	}

	logger.Debug().Msgf("Adding review checklist to Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	if _, _, err := client.Issues.CreateComment(ctx, prInfo.repoOwner, prInfo.repoName, prInfo.num, &prComment); err != nil {
		logger.Error().Err(err).Msgf("Failed to comment the review checklist to Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	}
	return nil
}

func (h *PullRequestHandler) addLabels(ctx context.Context, event github.PullRequestEvent, prInfo prInformation) error {
	installationID := githubapp.GetInstallationIDFromEvent(&event)

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	ctx, logger := githubapp.PreparePRContext(ctx, installationID, prInfo.repo, event.GetNumber())

	logger.Debug().Msgf("Adding initial labels to Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	if _, _, err := client.Issues.AddLabelsToIssue(ctx, prInfo.repoOwner, prInfo.repoName, prInfo.num, alwaysAddLabels); err != nil {
		logger.Error().Err(err).Msgf("Failed to add initial labels to Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	}
	return nil
}

func (h *PullRequestHandler) createErrorDocumentation(ctx context.Context, event github.PullRequestEvent, prInfo prInformation) error {
	installationID := githubapp.GetInstallationIDFromEvent(&event)

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	ctx, logger := githubapp.PreparePRContext(ctx, installationID, prInfo.repo, event.GetNumber())

	if prInfo.repoName != "vitess" {
		logger.Debug().Msgf("Pull Request %s/%s#%d is not on a vitess repo, skipping error generation", prInfo.repoOwner, prInfo.repoName, prInfo.num)
		return nil
	}

	logger.Debug().Msgf("Listing changed files in Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	changeDetected, err := detectErrorCodeChanges(ctx, prInfo, client)
	if err != nil {
		logger.Err(err).Msg(err.Error())
		return nil
	}
	if !changeDetected {
		logger.Debug().Msgf("No change detect to 'go/vt/vterrors/code.go' in Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
		return nil
	}
	logger.Debug().Msgf("Change detect to 'go/vt/vterrors/code.go' in Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)

	vterrorsgenVitess, err := cloneVitessAndGenerateErrors(prInfo)
	if err != nil {
		logger.Err(err).Msg(err.Error())
		return nil
	}

	currentVersionDocs, err := cloneWebsiteAndGetCurrentVersionOfDocs(prInfo)
	if err != nil {
		logger.Err(err).Msg(err.Error())
		return nil
	}

	errorDocContent, docPath, err := generateErrorCodeDocumentation(ctx, client, prInfo, currentVersionDocs, vterrorsgenVitess)
	if err != nil {
		logger.Err(err).Msg(err.Error())
		return nil
	}
	if errorDocContent == "" {
		logger.Debug().Msgf("No change detected in error code in Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
		return nil
	}

	err = createCommitAndPullRequestForErrorCode(ctx, prInfo, client, logger, errorDocContent, docPath)
	if err != nil {
		logger.Err(err).Msg(err.Error())
	}
	return nil
}

func (h *PullRequestHandler) backportPR(ctx context.Context, event github.PullRequestEvent, prInfo prInformation) error {
	installationID := githubapp.GetInstallationIDFromEvent(&event)

	client, err := h.NewInstallationClient(installationID)
	if err != nil {
		return err
	}

	ctx, logger := githubapp.PreparePRContext(ctx, installationID, prInfo.repo, event.GetNumber())

	pr, _, err := client.PullRequests.Get(ctx, prInfo.repoOwner, prInfo.repoName, prInfo.num)
	if err != nil {
		logger.Error().Err(err).Msgf("Failed to get Pull Request %s/%s#%d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
		return nil
	}

	var (
		backportBranches    []string // list of branches to which we must backport
		forwardportBranches []string // list of branches to which we must forward-port
		otherLabels         []string // will be used to apply the original PR's labels to the new PRs
	)
	for _, label := range pr.Labels {
		if label == nil {
			continue
		}
		if strings.HasPrefix(label.GetName(), backportLabelPrefix) {
			backportBranches = append(backportBranches, strings.Split(label.GetName(), backportLabelPrefix)[1])
		} else if strings.HasPrefix(label.GetName(), forwardportLabelPrefix) {
			forwardportBranches = append(forwardportBranches, strings.Split(label.GetName(), forwardportLabelPrefix)[1])
		} else {
			otherLabels = append(otherLabels, label.GetName())
		}
	}

	mergedCommitSHA := pr.GetMergeCommitSHA()

	for _, branch := range backportBranches {
		_, err = portPR(ctx, client, prInfo, pr, mergedCommitSHA, branch, backport, otherLabels)
		if err != nil {
			logger.Err(err).Msg(err.Error())
			continue
		}
	}
	for _, branch := range forwardportBranches {
		_, err = portPR(ctx, client, prInfo, pr, mergedCommitSHA, branch, forwardport, otherLabels)
		if err != nil {
			logger.Err(err).Msg(err.Error())
			continue
		}
	}

	return nil
}
