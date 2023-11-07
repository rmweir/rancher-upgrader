package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"helm.sh/helm/v3/pkg/action"
	cli2 "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/repo"
)

const (
	rancherReleaseNotesPrefix    = "https://api.github.com/repos/rancher/rancher/releases/tags/"
	majorBugFixHeader            = "# Major Bug Fixes"
	rancherBehaviorChangesHeader = "# Rancher Behavior Changes"
	knownIssuesHeader            = "# Known Issues"
	installUpgradeNotesHeader    = "# Install/Upgrade Notes"
)

var (
	markdownCommentsReg = regexp.MustCompile("<!--[A-Za-z0-9-#/, ]*-->")
)

type upgradeClient struct {
	actionConfig   *action.Configuration
	repoConfigPath string
	repoCachePath  string
}

func UpgradeCommand() *cli.Command {
	flags := []cli.Flag{
		&cli.StringFlag{
			Name:     "kubeconfig",
			Usage:    "Specify kubeconfig path",
			Value:    "",
			EnvVars:  []string{"KUBECONFIG"},
			Required: true,
		},
	}

	return &cli.Command{
		Name:   "upgrade",
		Usage:  "Bring the cluster up",
		Action: UpgradeRancher,
		Flags:  flags,
	}
}
func UpgradeRancher(ctx *cli.Context) error {
	fmt.Println("Detecting rancher releases...")
	kcPath := ctx.String("kubeconfig")
	client := newUpgradeClient(kcPath)
	helmActionConfig := client.actionConfig
	releases, err := action.NewList(helmActionConfig).Run()
	if err != nil {
		return err
	}

	currentVersion := ""
	for _, release := range releases {
		if release.Chart.Metadata.Name == "rancher" {
			fmt.Printf("release [%s] in namespace [%s] is a rancher install\n", release.Name, release.Namespace)
			currentVersion = release.Chart.Metadata.Version
		}
	}

	rancherStableRepo, err := client.verifyRancherStableRepoExists()
	if err != nil {
		return err
	}

	if err := client.updateRepositories(); err != nil {
		return err
	}

	indexFile, err := repo.LoadIndexFile(filepath.Join(client.repoCachePath, filepath.Join(helmpath.CacheIndexFile(rancherStableRepo.Name))))
	if err != nil {
		return err
	}

	latestStableRancherVersion, err := indexFile.Get("rancher", "")
	if err != nil {
		return err
	}

	fmt.Printf("would you like to update rancher from version [%s] to version [%s]?\n", currentVersion, latestStableRancherVersion.Version)
	// TODO: accept input here

	releaseSemverStrings, err := getReleasesBetweenInclusive(currentVersion, latestStableRancherVersion.Version)
	if err != nil {
		return err
	}

	bugfixes, knownIssues, err := walkthroughReleaseNotes(releaseSemverStrings)
	if err != nil {
		return err
	}

	/* TODO: walkthrough bugfixes
	example:
	"Let's review major bug fixes on the way from vA to vB"
	"Major Bug Fixes from vA to VA + 1:
	* bugfix1
	* bugfix2"
	*/
	/* TODO: walkthrough knownIssues
	example:
	"Let's walk through known issues one at a time"
	"knownissue1
	would you still like to proceed"
	*/
	fmt.Printf("%v %v", bugfixes, knownIssues)
	fmt.Printf("all releases inclusive: %v", releaseSemverStrings)
	return nil
}

func newUpgradeClient(kubeconfigPath string) upgradeClient {
	actionConfig := new(action.Configuration)

	settings := cli2.New()
	settings.KubeConfig = kubeconfigPath

	if err := actionConfig.Init(settings.RESTClientGetter(), "", os.Getenv("HELM_DRIVER"), logrus.Debugf); err != nil {
		os.Exit(1)
	}
	return upgradeClient{
		actionConfig:   actionConfig,
		repoConfigPath: settings.RepositoryConfig,
		repoCachePath:  settings.RepositoryCache,
	}
}

func getReleasesBetweenInclusive(startingRelease, finalRelease string) ([]string, error) {
	startingSemver, err := semver.New(startingRelease)
	if err != nil {
		return nil, err
	}
	finalSemver, err := semver.New(finalRelease)
	if err != nil {
		return nil, err
	}

	diff := finalSemver.Patch - startingSemver.Patch
	releases := make([]string, diff+1)
	for i := uint64(0); i < diff+1; i++ {
		releases[i] = fmt.Sprintf("%d.%d.%d", startingSemver.Major, startingSemver.Minor, startingSemver.Patch+i)
	}
	return releases, nil
}

func walkthroughReleaseNotes(releases []string) ([][]string, [][]string, error) {
	bugfixes := make([][]string, len(releases))
	knownIssues := make([][]string, len(releases))

	var recentBugfixAddition, recentKnownIssuesAddition string
	lastReleaseBugfixes := ""
	lastReleaseKnownIssues := ""
	for index, release := range releases {
		releaseNotes, err := getReleaseNotes(release)
		if err != nil {
			return nil, nil, err
		}
		// remove comments
		releaseNotes = markdownCommentsReg.ReplaceAllString(releaseNotes, "")

		fullBugfixBody, err := parseNotesSections(majorBugFixHeader, rancherBehaviorChangesHeader, releaseNotes)
		if err != nil {
			return nil, nil, err
		}
		if lastReleaseBugfixes != "" {
			recentBugfixAddition = strings.Replace(fullBugfixBody, lastReleaseBugfixes, "", 1)
		} else {
			recentBugfixAddition = fullBugfixBody
		}
		lastReleaseBugfixes = fullBugfixBody
		bugfixes[index] = parseBulletPoints(recentBugfixAddition)

		fullKnownIssuesBody, err := parseNotesSections(knownIssuesHeader, installUpgradeNotesHeader, releaseNotes)
		if err != nil {
			return nil, nil, err
		}
		if lastReleaseKnownIssues != "" {
			recentKnownIssuesAddition = strings.Replace(fullKnownIssuesBody, lastReleaseKnownIssues, "", 1)
		} else {
			recentKnownIssuesAddition = fullKnownIssuesBody
		}
		lastReleaseKnownIssues = fullKnownIssuesBody
		knownIssues[index] = parseBulletPoints(recentKnownIssuesAddition)
	}
	return bugfixes, knownIssues, nil
}

func getReleaseNotes(release string) (string, error) {
	releaseURL := fmt.Sprintf("%sv%s", rancherReleaseNotesPrefix, release)
	resp, err := http.Get(releaseURL)
	if err != nil {
		return "", err
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	// parsing json is forgone here as it does not reduce the amount of processing needed
	body := string(bodyBytes)

	return body, nil
}

func parseNotesSections(header1, header2, notes string) (string, error) {
	// bugfixBody := body[strings.Index(body, majorBugFixHeader)+len(majorBugFixHeader) : strings.Index(body, rancherBehaviorChangesHeader)]
	startIndex := strings.Index(notes, header1)
	stopIndex := strings.Index(notes, header2)
	if startIndex == -1 || stopIndex == -1 {
		return "", nil
	}
	sectionBody := notes[strings.Index(notes, header1)+len(header1) : strings.Index(notes, header2)]
	sectionBody = strings.ReplaceAll(sectionBody, "\\r\\n", "")
	/*
		if lastReleaseBugfixes != "" {
			bugfixBody = strings.Replace(bugfixBody, lastReleaseBugfixes, "", 1)
		}*/
	return sectionBody, nil
}

func parseBulletPoints(section string) []string {
	lines := strings.Split(section, "- ")
	bullets := make([]string, 0)
	for _, line := range lines {
		bullets = append(bullets, line)
	}
	return bullets
}

func (c *upgradeClient) updateRepositories() error {
	manager := downloader.Manager{
		RepositoryCache:  c.repoCachePath,
		RepositoryConfig: c.repoConfigPath,
		Out:              os.Stdout,
		Getters: getter.Providers{getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter,
		}},
	}

	return manager.UpdateRepositories()
}

func (c *upgradeClient) verifyRancherStableRepoExists() (*repo.Entry, error) {
	f, err := repo.LoadFile(c.repoConfigPath)
	if err != nil {
		return nil, err
	}
	for _, repo := range f.Repositories {
		isRancherStableRepo := strings.HasSuffix(strings.TrimSuffix(repo.URL, "/"), "releases.rancher.com/server-charts/stable")
		if isRancherStableRepo {
			return repo, nil
		}
	}

	return nil, fmt.Errorf("no repository found matach \"releases.rancher.com/server-charts/stable\"")
}
