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
	"fmt"
	"strings"

	"github.com/google/go-github/v53/github"
	"github.com/pkg/errors"
)

func portPR(
	ctx context.Context,
	client *github.Client,
	prInfo prInformation,
	pr *github.PullRequest,
	mergedCommitSHA, branch, portType string,
	labels []string,
) (int, error) {
	// Get a reference to the release branch
	releaseRef, _, err := client.Git.GetRef(ctx, prInfo.repoOwner, prInfo.repoName, fmt.Sprintf("heads/%s", branch))
	if err != nil {
		return 0, errors.Wrapf(err, "")
	}

	// Create a new branch from the release branch
	newBranch := fmt.Sprintf("%s-%d-to-%s", portType, pr.GetNumber(), branch)
	newRef := &github.Reference{
		Ref: github.String("refs/heads/" + newBranch),
		Object: &github.GitObject{
			SHA: releaseRef.GetObject().SHA,
		},
	}
	_, _, err = client.Git.CreateRef(ctx, prInfo.repoOwner, prInfo.repoName, newRef)
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to create git ref %s on repository %s/%s to backport Pull Request %d", newBranch, prInfo.repoOwner, prInfo.repoName, prInfo.num)
	}

	// Clone the repository
	_, err = execCmd("", "git", "clone", fmt.Sprintf("git@github.com:%s/%s.git", prInfo.repoOwner, prInfo.repoName), "/tmp/vitess")
	if err != nil && !strings.Contains(err.Error(), "already exists and is not an empty directory") {
		return 0, errors.Wrapf(err, "Failed to clone repository %s/%s to backport Pull Request %d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	}

	// Fetch origin
	_, err = execCmd("/tmp/vitess", "git", "fetch", "origin")
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to fetch origin on repository %s/%s to backport Pull Request %d", prInfo.repoOwner, prInfo.repoName, prInfo.num)
	}

	// Checkout the new branch
	_, err = execCmd("/tmp/vitess", "git", "checkout", newBranch)
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to checkout repository %s/%s to branch %s to backport Pull Request %d", prInfo.repoOwner, prInfo.repoName, newBranch, prInfo.num)
	}

	conflict := false

	// Cherry-pick the commit
	_, err = execCmd("/tmp/vitess", "git", "cherry-pick", "-m", "1", mergedCommitSHA)
	if err != nil && strings.Contains(err.Error(), "conflicts") {
		_, err = execCmd("/tmp/vitess", "git", "add", ".")
		if err != nil {
			return 0, errors.Wrapf(err, "Failed to do 'git add' on branch %s to backport Pull Request %d", newBranch, prInfo.num)
		}

		_, err = execCmd("/tmp/vitess", "git", "commit", "--author=\"vitess-bot[bot] <108069721+vitess-bot[bot]@users.noreply.github.com>\"", "-m", fmt.Sprintf("Cherry-pick %s with conflicts", mergedCommitSHA))
		if err != nil {
			return 0, errors.Wrapf(err, "Failed to do 'git commit' on branch %s to backport Pull Request %d", newBranch, prInfo.num)
		}

		conflict = true
	} else if err != nil {
		return 0, errors.Wrapf(err, "Failed to cherry-pick %s to branch %s to backport Pull Request %d", mergedCommitSHA, newBranch, prInfo.num)
	} else {
		_, err = execCmd("/tmp/vitess", "git", "commit", "--amend", "--author=\"vitess-bot[bot] <108069721+vitess-bot[bot]@users.noreply.github.com>\"", "--no-edit")
		if err != nil {
			return 0, errors.Wrapf(err, "Failed to do 'git commit --amend' on branch %s to backport Pull Request %d", newBranch, prInfo.num)
		}
	}

	// Push the changes
	_, err = execCmd("/tmp/vitess", "git", "push", "origin", newBranch)
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to push %s to backport Pull Request %s", newBranch, prInfo.num)
	}

	// Create a Pull Request for the new branch
	newPR := &github.NewPullRequest{
		Title:               github.String(fmt.Sprintf("[%s] %s (#%d)", branch, pr.GetTitle(), pr.GetNumber())),
		Head:                github.String(newBranch),
		Base:                github.String(branch),
		Body:                github.String(fmt.Sprintf("## Description\nThis is a %s of #%d", portType, pr.GetNumber())),
		MaintainerCanModify: github.Bool(true),
		Draft:               &conflict,
	}
	newPRCreated, _, err := client.PullRequests.Create(ctx, prInfo.repoOwner, prInfo.repoName, newPR)
	if err != nil {
		return 0, errors.Wrapf(err, "")
	}

	labelsToAdd := labels
	if conflict {
		labelsToAdd = append(labelsToAdd, "Merge Conflict", "Skip CI")
	}
	switch portType {
	case backport:
		labelsToAdd = append(labelsToAdd, "Backport")
	case forwardport:
		labelsToAdd = append(labelsToAdd, "Forwardport")
	}

	newPRNumber := newPRCreated.GetNumber()
	if _, _, err = client.Issues.AddLabelsToIssue(ctx, prInfo.repoOwner, prInfo.repoName, newPRNumber, labelsToAdd); err != nil {
		return 0, errors.Wrapf(err, "Failed to assign labels to Pull Request %d", newPRNumber)
	}

	originalPRAuthor := pr.GetUser().GetLogin()
	if conflict {
		conflictCommentBody := fmt.Sprintf(
			"Hello @%s, there are conflicts in this %s.\n\nPlease addresse them in order to merge this Pull Request. You can execute the snippet below to reset your branch and resolve the conflict manually.\n\nMake sure you replace `origin` by the name of the %s/%s remote \n```\ngit fetch --all\ngh pr checkout %d -R %s/%s\ngit reset --hard origin/%s\ngit cherry-pick -m 1 %s\n",
			originalPRAuthor,
			portType,
			prInfo.repoOwner,
			prInfo.repoName,
			newPRNumber,
			prInfo.repoOwner,
			prInfo.repoName,
			branch,
			mergedCommitSHA,
		)
		prCommentConflict := github.IssueComment{
			Body: &conflictCommentBody,
		}
		if _, _, err := client.Issues.CreateComment(ctx, prInfo.repoOwner, prInfo.repoName, newPRNumber, &prCommentConflict); err != nil {
			return 0, errors.Wrapf(err, "Failed to comment conflict notice on Pull Request %d", newPRNumber)
		}
	}

	oldReviewers, _, err := client.PullRequests.ListReviewers(ctx, prInfo.repoOwner, prInfo.repoName, prInfo.num,  nil)
	if err != nil {
		return 0, errors.Wrapf(err, "Failed to get the list of reviewers on Pull Request %d", prInfo.num)
	}

	var reviewers []string
	for _, user := range oldReviewers.Users {
		reviewers = append(reviewers, user.GetLogin())
	}
	for _, team := range oldReviewers.Teams {
		reviewers = append(reviewers, team.GetName())
	}
	reviewers = append(reviewers, originalPRAuthor)
	_, _, err = client.PullRequests.RequestReviewers(ctx, prInfo.repoOwner, prInfo.repoName, newPRNumber, github.ReviewersRequest{
		Reviewers: reviewers,
	})
	if err != nil {
		return 0, errors.Wrapf(err, "")
	}

	return newPRNumber, nil
}
