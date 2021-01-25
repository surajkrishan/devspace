package upgrade

import (
	"errors"
	"os"
	"regexp"
	"sync"

	"github.com/loft-sh/devspace/pkg/util/log"

	"github.com/blang/semver"
	"github.com/rhysd/go-github-selfupdate/selfupdate"
)

// Version holds the current version tag
var version string
var rawVersion string

var githubSlug = "loft-sh/devspace"
var reVersion = regexp.MustCompile(`\d+\.\d+\.\d+`)

func eraseVersionPrefix(version string) (string, error) {
	indices := reVersion.FindStringIndex(version)
	if indices == nil {
		return version, errors.New("Version not adopting semver")
	}
	if indices[0] > 0 {
		version = version[indices[0]:]
	}

	return version, nil
}

// PrintUpgradeMessage prints an upgrade message if there is a new version available
func PrintUpgradeMessage() {
	if os.Getenv("DEVSPACE_SKIP_VERSION_CHECK") != "true" {
		// Get version of current binary
		latestVersion := NewerVersionAvailable()
		if latestVersion != "" {
			log.GetInstance().Warnf("There is a newer version of DevSpace: v%s. Run `devspace upgrade` to upgrade to the newest version.\n", latestVersion)
		}
	}
}

// GetVersion returns the application version
func GetVersion() string {
	return version
}

// GetRawVersion returns the applications raw version
func GetRawVersion() string {
	return rawVersion
}

// SetVersion sets the application version
func SetVersion(verText string) {
	if len(verText) > 0 {
		if verText[0] != 'v' {
			verText = "v" + verText
		}

		_version, err := eraseVersionPrefix(verText)
		if err != nil {
			log.GetInstance().Errorf("Error parsing version: %v", err)
			return
		}

		version = _version
		rawVersion = verText
	}
}

var (
	latestVersion     string
	latestVersionErr  error
	latestVersionOnce sync.Once
)

// CheckForNewerVersion checks if there is a newer version on github and returns the newer version
func CheckForNewerVersion() (string, error) {
	latestVersionOnce.Do(func() {
		latest, found, err := selfupdate.DetectLatest(githubSlug)
		if err != nil {
			latestVersionErr = err
			return
		}

		v := semver.MustParse(version)
		if !found || latest.Version.Equals(v) {
			return
		}

		latestVersion = latest.Version.String()
	})

	return latestVersion, latestVersionErr
}

// NewerVersionAvailable checks if there is a newer version of devspace
func NewerVersionAvailable() string {
	// Get version of current binary
	version := GetVersion()
	if version != "" {
		latestStableVersion, err := CheckForNewerVersion()
		if latestStableVersion != "" && err == nil { // Check versions only if newest version could be determined without errors
			semverVersion, err := semver.Parse(version)
			if err == nil { // Only compare version if version can be parsed
				semverLatestStableVersion, err := semver.Parse(latestStableVersion)
				if err == nil { // Only compare version if latestStableVersion can be parsed
					// If latestStableVersion > version
					if semverLatestStableVersion.Compare(semverVersion) == 1 {
						return latestStableVersion
					}
				}
			}
		}
	}

	return ""
}

// Upgrade downloads the latest release from github and replaces devspace if a new version is found
func Upgrade() error {
	log := log.GetInstance()

	newerVersion, err := CheckForNewerVersion()
	if err != nil {
		return err
	}
	if newerVersion == "" {
		log.Infof("Current binary is the latest version: %s", version)
		return nil
	}

	v := semver.MustParse(version)

	log.StartWait("Downloading newest version...")
	latest, err := selfupdate.UpdateSelf(v, githubSlug)
	log.StopWait()
	if err != nil {
		return err
	}

	if latest.Version.Equals(v) {
		// latest version is the same as current version. It means current binary is up to date.
		log.Infof("Current binary is the latest version: %s", version)
	} else {
		log.Donef("Successfully updated to version %s", latest.Version)
		log.Infof("Release note: \n\n%s", latest.ReleaseNotes)
	}

	return nil
}
