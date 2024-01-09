package dashboardassets

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/viper"
	filehelpers "github.com/turbot/go-kit/files"
	"github.com/turbot/pipe-fittings/app_specific"
	"github.com/turbot/pipe-fittings/filepaths"
	"github.com/turbot/pipe-fittings/ociinstaller"
	"github.com/turbot/pipe-fittings/statushooks"
	"github.com/turbot/powerpipe/internal/constants"
	"github.com/turbot/steampipe-plugin-sdk/v5/sperr"
)

const (
	EnvAssetsLookup = "POWERPIPE_ASSETS_LOOKUP" // an environment which disables fetching and verification of assets
)

func Ensure(ctx context.Context) error {
	slog.Info("dashboardassets.Ensure start")
	defer slog.Info("dashboardassets.Ensure end")

	reportAssetsPath := filepaths.EnsureDashboardAssetsDir()
	isLocalBuild := viper.GetString(constants.ConfigKeyBuiltBy) == constants.LocalBuild

	slog.Info("dashboardassets.Ensure", "reportAssetsPath", reportAssetsPath, "isLocalBuild", isLocalBuild)

	// this is false when this binary is built by goreleaser
	if !isLocalBuild {
		if lookup, ok := os.LookupEnv(EnvAssetsLookup); ok && strings.ToLower(lookup) == "disabled" {
			slog.Info("dashboardassets.Ensure", "EnvAssetsLookup", lookup)
			// assets lookup is disabled
			return nil
		}

		if installedAsstesMatchAppVersion() {
			// this is a released version and the version of the assets matches the version of the app
			return nil
		}
		statushooks.SetStatus(ctx, "Installing dashboard server…")
		// there is a version mismatch - we need to download and install the assets of this version
		return downloadReleasedAssets(ctx, reportAssetsPath, app_specific.AppVersion)
	}

	// check that the assets are already installed
	if !filehelpers.DirectoryExists(reportAssetsPath) {
		// assets are not installed - error out
		return sperr.New("dashboard assets need to be preinstalled in %s when developing", reportAssetsPath)
	}

	return nil
}

func downloadReleasedAssets(ctx context.Context, location string, version *semver.Version) error {
	slog.Info("dashboardassets.downloadReleasedAssets start")
	defer slog.Info("dashboardassets.downloadReleasedAssets end")

	// get the list of releases
	releases, err := getReleases()
	if err != nil {
		return sperr.WrapWithMessage(err, "could not fetch release assets")
	}
	var release *Release
	for _, r := range releases {
		// parse this as a semver
		// we do this to make it more resilient against the 'v' prefix
		// which may or may not be present in either
		releaseVersion, err := semver.NewVersion(r.Name)
		if err != nil {
			// we couldn't parse this version - skip it
			slog.Info("Could not parse release version", "version", r.Name, "error", err)
			continue
		}
		if version.Equal(releaseVersion) {
			release = r
			break
		}
	}
	if release == nil {
		return sperr.New("could not find assets for release %s", version)
	}

	return downloadAndInstallAssets(ctx, release, location)
}

func downloadAndInstallAssets(ctx context.Context, release *Release, location string) error {
	slog.Info("dashboardassets.downloadAndInstallAssets start")
	defer slog.Info("dashboardassets.downloadAndInstallAssets end")

	tempDir := ociinstaller.NewTempDir(location)
	defer func() {
		if err := tempDir.Delete(); err != nil {
			slog.Info("Failed to delete temp dir after installing assets", "tempDir", tempDir, "error", err)
		}
	}()
	// download the assets
	asset := release.getDashboardAsset()
	if asset == nil {
		return sperr.New("could not find dashboard asset in release")
	}

	filePath := filepath.Join(location, "assets.tar.gz")
	// download the assets
	err := downloadFile(filePath, asset.Url)
	if err != nil {
		return sperr.WrapWithMessage(err, "could not download dashboard assets")
	}

	// remove the file after we are done
	defer os.Remove(filePath)

	statushooks.SetStatus(ctx, "Extracting dashboard server…")
	err = extractTarGz(ctx, filePath, location)
	if err != nil {
		return sperr.WrapWithMessage(err, "could not extract dashboard assets")
	}
	return nil
}

func installedAsstesMatchAppVersion() bool {
	versionFile, err := loadReportAssetVersionFile()
	if err != nil {
		return false
	}

	slog.Info("installedAsstesMatchAppVersion", "versionFile", versionFile, "app_specific.AppVersion", app_specific.AppVersion.String())

	currentAppVersion, err := semver.NewVersion(app_specific.AppVersion.String())
	if err != nil {
		// if we couldn't parse this, assume we need to download
		return false
	}
	currentAssetVersion, err := semver.NewVersion(versionFile.Version)
	if err != nil {
		// if we couldn't parse this, assume we need to download
		return false
	}

	slog.Info("installedAsstesMatchAppVersion", "currentAppVersion", currentAppVersion, "currentAssetVersion", currentAssetVersion, "equal", currentAppVersion.Equal(currentAssetVersion))

	return currentAppVersion.Equal(currentAssetVersion)
}

type ReportAssetsVersionFile struct {
	Version string `json:"version"`
}

func loadReportAssetVersionFile() (*ReportAssetsVersionFile, error) {
	versionFilePath := filepaths.ReportAssetsVersionFilePath()
	if !filehelpers.FileExists(versionFilePath) {
		return &ReportAssetsVersionFile{}, nil
	}

	file, _ := os.ReadFile(versionFilePath)
	var versionFile ReportAssetsVersionFile
	if err := json.Unmarshal(file, &versionFile); err != nil {
		slog.Error("Error while reading dashboard assets version file", "error", err)
		return nil, err
	}

	return &versionFile, nil

}

// extractTarGz extracts a .tar.gz archive to a destination directory.
// this can go into pipe-fittings
// TODO::Binaek - move this to pipe-fittings
func extractTarGz(ctx context.Context, assetTarGz string, dest string) error {
	slog.Info("dashboardassets.extractTarGz start")
	defer slog.Info("dashboardassets.extractTarGz end")

	gzipStream, err := os.Open(assetTarGz)
	if err != nil {
		return sperr.WrapWithMessage(err, "could not open dashboard assets archive")
	}
	defer gzipStream.Close()

	uncompressedStream, err := gzip.NewReader(gzipStream)
	if err != nil {
		return err
	}
	uncompressedStream.Close()

	tarReader := tar.NewReader(uncompressedStream)

	for {
		header, err := tarReader.Next()

		switch {
		case err == io.EOF:
			return nil
		case err != nil:
			return err
		case header == nil:
			continue
		}

		//nolint:gosec // TODO G110: Potential DoS vulnerability via decompression bomb
		target := filepath.Join(dest, header.Name)

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			statushooks.SetStatus(ctx, fmt.Sprintf("Extracting %s…", header.Name))
			outFile, err := os.Create(target)
			if err != nil {
				return err
			}
			//nolint:gosec // TODO G110: Potential DoS vulnerability via decompression bomb
			if _, err := io.Copy(outFile, tarReader); err != nil {
				outFile.Close()
				return err
			}
			outFile.Close()
		default:
			return sperr.New("ExtractTarGz: uknown type: %b in %s", header.Typeflag, header.Name)
		}
	}
}
