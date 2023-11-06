package cmd

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
	"helm.sh/helm/v3/pkg/action"
	cli2 "helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/downloader"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/helmpath"
	"helm.sh/helm/v3/pkg/repo"
	"os"
	"path/filepath"
	"strings"
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
