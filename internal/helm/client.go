package helm

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/blang/semver/v4"
	"github.com/enescakir/emoji"
	"github.com/sirupsen/logrus"
	"helm.sh/helm/v3/pkg/action"
	cli2 "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
)

type Client struct {
	actionConfig *action.Configuration
	index        *repo.IndexFile
}

func NewClient(kubeconfigPath string) (Client, error) {
	actionConfig := new(action.Configuration)

	settings := cli2.New()
	settings.KubeConfig = kubeconfigPath

	if err := actionConfig.Init(settings.RESTClientGetter(), "", os.Getenv("HELM_DRIVER"), logrus.Debugf); err != nil {
		os.Exit(1)
	}

	rancherStableRepo, err := verifyRancherStableRepoExists(settings.RepositoryConfig)
	if err != nil {
		return Client{}, err
	}

	if err := updateRepositories(settings.RepositoryCache, settings.RepositoryConfig); err != nil {
		return Client{}, err
	}

	index, err := repo.LoadIndexFile(filepath.Join(settings.RepositoryCache, filepath.Join(helmpath.CacheIndexFile(rancherStableRepo.Name))))
	if err != nil {
		return Client{}, err
	}

	return Client{
		actionConfig: actionConfig,
		index:        index,
	}, nil
}

func (c Client) ListReleases() ([]*release.Release, error) {
	helmActionConfig := c.actionConfig
	releases, err := action.NewList(helmActionConfig).Run()
	if err != nil {
		return nil, err
	}
	return releases, err
}

func (c Client) FindRancherRelease() (*release.Release, error) {
	releases, err := c.ListReleases()
	if err != nil {
		return nil, err
	}

	for _, release := range releases {
		if release.Chart.Metadata.Name == "rancher" {
			fmt.Printf("Found rancher release [%s] in namespace [%s]\n", release.Name, release.Namespace)
			fmt.Printf("Is %s:%s the rancher release you would like to upgrade?\n", release.Name, release.Namespace)
			return release, nil
		}
	}
	return nil, fmt.Errorf("rancher release could not be found")
}

func verifyRancherStableRepoExists(repoConfigPath string) (*repo.Entry, error) {
	fmt.Println("Verifying rancher-stable repo exists...")
	f, err := repo.LoadFile(repoConfigPath)
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

func updateRepositories(repoCachePath, repoConfigPath string) error {
	manager := downloader.Manager{
		RepositoryCache:  repoCachePath,
		RepositoryConfig: repoConfigPath,
		Out:              os.Stdout,
		Getters: getter.Providers{getter.Provider{
			Schemes: []string{"http", "https"},
			New:     getter.NewHTTPGetter,
		}},
	}

	return manager.UpdateRepositories()
}

func (c Client) GetNextSupportedRancherChartVersion(currentVersion string) (string, error) {
	currentChartVersion, err := semver.New(currentVersion)
	if err != nil {
		return "", err
	}

	c.index.SortEntries()
	nextMinorUpgrade := ""
	latestPatchOnCurrentMinorVersion := ""
	for _, chartVersion := range c.index.Entries["rancher"] {
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

func (c Client) GetRancherChartForVersion(version string) (*repo.ChartVersion, error) {
	return c.index.Get("rancher", version)
}

func (c Client) Upgrade(release *release.Release, overrideValues map[string]interface{}) (*release.Release, error) {
	upgradeAction := action.NewUpgrade(c.actionConfig)
	upgradeAction.DryRun = true

	newRelease, err := upgradeAction.Run(release.Name, release.Chart, overrideValues)
	if err != nil {
		return nil, err
	}
	return newRelease, nil
}
