package cmd

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/enescakir/emoji"
	"github.com/fatih/color"
	"github.com/ghodss/yaml"
	"github.com/rmweir/rancher-upgrader/internal/helm"
	"github.com/urfave/cli/v2"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chartutil"
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

type helmExecer interface {
	FindRancherRelease() (*release.Release, error)
	GetNextSupportedRancherChartVersion(currentVersion string) (string, error)
	GetRancherChartForVersion(version string) (*repo.ChartVersion, error)
	Upgrade(release *release.Release, overrideValues map[string]interface{}) (*release.Release, error)
}

type UpgradeActionClient struct {
	helmExecer helmExecer
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

	c := &UpgradeActionClient{}
	return &cli.Command{
		Name:   "upgrade",
		Usage:  "Bring the cluster up",
		Action: c.UpgradeRancher,
		Flags:  flags,
	}
}

func (u *UpgradeActionClient) Init(kubeconfigPath string) error {
	client, err := helm.NewClient(kubeconfigPath)
	if err != nil {
		return err
	}
	u.helmExecer = client
	return nil
}

func (u *UpgradeActionClient) UpgradeRancher(ctx *cli.Context) error {
	fmt.Printf("Welcome to rancher upgrader %v\n", emoji.CowboyHatFace)
	fmt.Printf("%v Detecting rancher releases...\n", emoji.MagnifyingGlassTiltedLeft)

	u.Init(ctx.String("kubeconfig"))

	targetRelease, err := u.helmExecer.FindRancherRelease()
	if err != nil {
		return err
	}
	currentVersion := targetRelease.Chart.Metadata.Version

	nextSupportedChartVersion, err := u.helmExecer.GetNextSupportedRancherChartVersion(targetRelease.Chart.Metadata.Version)
	if err != nil {
		return err
	}

	if currentVersion == nextSupportedChartVersion {
		fmt.Printf("%v Your rancher install is already up to date!", emoji.PartyingFace)
		return nil
	}

	latestStableRancherChart, err := u.helmExecer.GetRancherChartForVersion(nextSupportedChartVersion)
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

	releaseSemverStrings, err := getReleasesBetweenInclusive(currentVersion, latestStableRancherChart.Version)
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

	fmt.Println()
	overrideValues, err := chartValuesPrompt(targetRelease.Chart, targetRelease.Config, reader)
	if err != nil {
		return err
	}

	targetRelease.Chart.Metadata.Version = latestStableRancherChart.Version
	newRelease, err := u.helmExecer.Upgrade(targetRelease, overrideValues)
	if err != nil {
		return err
	}

	fmt.Printf("%v%v You have succesfully upgraded rancher from version [%s] to version [%s]!\n", emoji.PartyPopper, emoji.Fireworks, currentVersion, newRelease.Chart.Metadata.Version)

	return nil
}

func chartValuesPrompt(chart *chart.Chart, values map[string]interface{}, reader *bufio.Reader) (map[string]interface{}, error) {
	var done bool
	for !done {
		if len(values) != 0 {
			valuesYAMLBytes, err := yaml.Marshal(values)
			if err != nil {
				return nil, err
			}
			fmt.Println("Here are the current chart override values:")
			fmt.Printf("%s\n", string(valuesYAMLBytes))
		} else {
			fmt.Println("There are currently no chart override values configured.")
		}
		answer := ""
		var err error
		// make this into a function for any y/n question
		for answer == "" {
			fmt.Print("Would you like to see all configured values, including defaults? [y/n]")
			answer, err = reader.ReadString('\n')
			if err != nil {
				return nil, err
			}

			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer == "n" || answer == "y" {
				break
			}
			fmt.Println("\nInvalid input, try again.")
		}
		if answer == "y" {
			coalescedValues, err := chartutil.CoalesceValues(chart, values)
			if err != nil {
				return nil, err
			}
			coalescedValuesYAMLBytes, err := yaml.Marshal(coalescedValues)
			if err != nil {
				return nil, err
			}
			fmt.Println("Values to be applied to rancher chart:")
			fmt.Println(string(coalescedValuesYAMLBytes))
		}
		answer = ""
		for answer == "" {
			fmt.Println("\nSelect one of the following options by entering their corresponding number")
			fmt.Println("1. Continue with displayed override chart values")
			fmt.Println("2. Configure different override values")
			answer, err = reader.ReadString('\n')
			if err != nil {
				return nil, err
			}
			answer = strings.ToLower(strings.TrimSpace(answer))
			if answer == "1" {
				done = true
				break
			}

			if answer == "2" {
				values, err = uploadValuesPrompt(reader)
				continue
			}
			fmt.Println("\nInvalid input, please try again.")
		}
	}
	return values, nil
}

func uploadValuesPrompt(reader *bufio.Reader) (map[string]interface{}, error) {
	fmt.Printf("Enter a filepath for a values.yaml file: ")
	filepath, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	file, err := os.Open(filepath)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	var values map[string]interface{}
	err = yaml.Unmarshal(data, &values)
	if err != nil {
		return nil, err
	}
	return values, nil
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

		answer = strings.ToLower(strings.TrimSpace(answer))
		if answer == "n" || answer == "y" {
			break
		}
		fmt.Println("\nInvalid input, try again.")
	}
	return answer == "y", nil
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
