package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/enescakir/emoji"
	"github.com/fatih/color"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"helm.sh/helm/v3/pkg/action"
	cli2 "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
)

const (
	ghReleaseNotesAPIPrefix      = "https://api.github.com/repos/rancher/rancher/releases/tags/"
	rancherReleaseNotesPrefix    = "https://github.com/rancher/rancher/releases/tag/"
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
	fmt.Printf("Welcome to rancher upgrader %v\n", emoji.CowboyHatFace)
	fmt.Printf("%v Detecting rancher releases...\n", emoji.MagnifyingGlassTiltedLeft)
	kcPath := ctx.String("kubeconfig")
	client := newUpgradeClient(kcPath)
	helmActionConfig := client.actionConfig
	releases, err := action.NewList(helmActionConfig).Run()
	if err != nil {
		return err
	}

	currentVersion := ""
	var targetRelease *release.Release
	for _, release := range releases {
		if release.Chart.Metadata.Name == "rancher" {
			fmt.Printf("Found rancher release [%s] in namespace [%s]\n", release.Name, release.Namespace)
			fmt.Printf("Is %s:%s the rancher release you would like to upgrade?\n", release.Name, release.Namespace)
			currentVersion = release.Chart.Metadata.Version
			targetRelease = release
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

	nextSupportedChartVersion, err := getNextSupportedChartVersion(currentVersion, indexFile)
	if err != nil {
		return err
	}

	if currentVersion == nextSupportedChartVersion {
		fmt.Printf("%v Your rancher install is already up to date!", emoji.PartyingFace)
		return nil
	}

	latestStableRancherChart, err := indexFile.Get("rancher", nextSupportedChartVersion)
	if err != nil {
		return err
	}

	fmt.Printf("Next available update from version [%s] to version [%s].\n", currentVersion, latestStableRancherChart.Version)

	reader := bufio.NewReader(os.Stdin)
	cont, err := promptForContinue(reader)
	if err != nil {
		return err
	}
	if !cont {
		return nil
	}

	releaseSemverStrings, err := getReleasesBetweenInclusive(targetRelease.Chart.Metadata.Version, latestStableRancherChart.Version)
	if err != nil {
		return err
	}

	bugfixes, knownIssues, err := parseReleaseNotes(releaseSemverStrings)
	if err != nil {
		return err
	}

	cont, err = walkthroughRelevantNotes(releaseSemverStrings, bugfixes, knownIssues, reader)
	if err != nil {
		return err
	}

	targetRelease.Chart.Metadata.Version = latestStableRancherChart.Version
	upgradeAction := action.NewUpgrade(helmActionConfig)
	upgradeAction.DryRun = true
	newRelease, err := upgradeAction.Run(targetRelease.Name, targetRelease.Chart, targetRelease.Chart.Values)
	if err != nil {
		return err
	}

	fmt.Printf("%v%v You have succesfully upgraded rancher from version [%s] to version [%s]!\n", emoji.PartyPopper, emoji.Fireworks, currentVersion, newRelease.Chart.Metadata.Version)

	return nil
}

func getNextSupportedChartVersion(currentVersion string, index *repo.IndexFile) (string, error) {
	currentChartVersion, err := semver.New(currentVersion)
	if err != nil {
		return "", err
	}

	index.SortEntries()
	nextMinorUpgrade := ""
	latestPatchOnCurrentMinorVersion := ""
	for _, chartVersion := range index.Entries["rancher"] {
		chartSemver, err := semver.New(chartVersion.Version)
		if err != nil {
			return "", err
		}
		if nextMinorUpgrade == "" && chartSemver.Minor-1 == currentChartVersion.Minor {
			nextMinorUpgrade = chartVersion.Version
			continue
		}
		if chartSemver.Minor != currentChartVersion.Minor {
			continue
		}
		latestPatchOnCurrentMinorVersion = chartVersion.Version
		break
	}

	if latestPatchOnCurrentMinorVersion == "" {
		// should always be able to detect latest patch for current minor version
		return "", fmt.Errorf("there was an issue detecting the next supported rancher chart version: could not"+
			"detect latest patch for line [%d.%d.x]", currentChartVersion.Major, currentChartVersion.Minor)
	}

	if currentVersion != latestPatchOnCurrentMinorVersion {
		return latestPatchOnCurrentMinorVersion, nil
	}

	if nextMinorUpgrade != "" {
		return nextMinorUpgrade, nil
	}

	// if the current version is equal to latest patch on that version's minor and there is no next minor upgrade,
	// the rancher install is up-to-date.
	return currentVersion, nil
}
func promptForContinue(reader *bufio.Reader) (bool, error) {
	var answer string
	var err error
	for answer == "" {
		fmt.Print("Continue? [y/n]")
		answer, err = reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		fmt.Printf(answer)
		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "n" || answer == "y" {
			break
		}
		fmt.Println("\nInvalid input, try again.")
	}
	return answer == "y", nil
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

func parseReleaseNotes(releases []string) ([][]string, [][]string, error) {
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

func walkthroughRelevantNotes(releases []string, bugfixes [][]string, knownIssues [][]string, reader *bufio.Reader) (bool, error) {
	fmt.Printf("There have been %d releases between rancher [%s] and rancher [%s] (inclusive).\n", len(releases)-1, releases[0], releases[len(releases)-1])
	fmt.Println("Let's go over the changes that have happened throughout these releases")
	for index, release := range releases {
		if index == len(release)-1 {
			break
		}
		nextReleaseIndex := index + 1
		fmt.Printf("%s -> %s\n", release, releases[nextReleaseIndex])
		cont, err := displayBugFixes(releases[nextReleaseIndex], bugfixes[nextReleaseIndex], reader)
		if err != nil {
			return false, err
		}
		if !cont {
			return false, nil
		}
		cont, err = displayKnownIssues(releases[nextReleaseIndex], knownIssues[nextReleaseIndex], reader)
		if err != nil {
			return false, err
		}
		if !cont {
			return false, nil
		}
	}
	return true, nil
}

func displayBugFixes(release string, bugfixes []string, reader *bufio.Reader) (bool, error) {
	var displayedOpeningMessage bool

	for _, bugfix := range bugfixes {
		if bugfix == "" || bugfix == "-->" {
			continue
		}
		if !displayedOpeningMessage {
			color.Green("Here are some of the bugfixes introduced by release [%s]", release)
			displayedOpeningMessage = true
		}
		fmt.Printf("%v %s\n", emoji.CheckMark, bugfix)
	}
	if !displayedOpeningMessage {
		fmt.Println("We did not find any bugfixes, we recommend consulting the release page for more info.")
	}
	fmt.Printf("If you would like to read more about bugfixes in release [%s], visit %sv%s\n", release, rancherReleaseNotesPrefix, release)
	return promptForContinue(reader)
}

func displayKnownIssues(release string, knownIssues []string, reader *bufio.Reader) (bool, error) {
	var displayedOpeningMessage bool

	for _, issue := range knownIssues {
		if issue == "" || issue == "-->" {
			continue
		}
		if !displayedOpeningMessage {
			fmt.Printf("Let's review the known issues in release [%s]\n", release)
			displayedOpeningMessage = true
		}
		fmt.Printf("%v  %s\n", emoji.RaisedHand, issue)
		fmt.Printf("Continue if you acknowledge this issue and still wish to proceed. ")
		cont, err := promptForContinue(reader)
		if err != nil {
			return false, err
		}
		if !cont {
			return false, nil
		}
	}
	if !displayedOpeningMessage {
		fmt.Printf("We did not find any known issues for release [%s].\n", release)
	}
	return true, nil
}

func getReleaseNotes(release string) (string, error) {
	releaseURL := fmt.Sprintf("%sv%s", ghReleaseNotesAPIPrefix, release)
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
	startIndex := strings.Index(notes, header1)
	stopIndex := strings.Index(notes, header2)
	if startIndex == -1 || stopIndex == -1 {
		return "", nil
	}
	sectionBody := notes[strings.Index(notes, header1)+len(header1) : strings.Index(notes, header2)]
	sectionBody = strings.ReplaceAll(sectionBody, "\\r\\n", "")

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
	fmt.Println("Verifying rancher-stable repo exists...")
	f, err := repo.LoadFile(c.repoConfigPath)
	if err != nil {
		return nil, err
	}
	for _, repo := range f.Repositories {
		isRancherStableRepo := strings.HasSuffix(strings.TrimSuffix(repo.URL, "/"), "releases.rancher.com/server-charts/stable")
		if isRancherStableRepo {
			fmt.Printf("%v Rancher-stable repo found!\n", emoji.ThumbsUp)
			return repo, nil
		}
	}

	return nil, fmt.Errorf("no repository found matach \"releases.rancher.com/server-charts/stable\"")
}
