package npm

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"sort"
	"time"

	"github.com/ossf/package-feeds/events"
	"github.com/ossf/package-feeds/feeds"
	"github.com/ossf/package-feeds/utils"
)

const (
	FeedName = "npm"
	rssPath  = "/-/rss"
)

var (
	httpClient = &http.Client{
		Timeout: 10 * time.Second,
	}
	errJSON        = errors.New("error unmarshaling json response internally")
	errUnpublished = errors.New("package is currently unpublished")
)

type Response struct {
	PackageEvents []PackageEvent `xml:"channel>item"`
}

type Package struct {
	Title       string
	CreatedDate time.Time
	Version     string
	Unpublished bool
}

type PackageEvent struct {
	Title string `xml:"title"`
}

// Returns a slice of PackageEvent{} structs.
func fetchPackageEvents(baseURL string) ([]PackageEvent, error) {
	pkgURL, err := utils.URLPathJoin(baseURL, rssPath)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Get(pkgURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	err = utils.CheckResponseStatus(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch npm package data: %w", err)
	}
	rssResponse := &Response{}
	reader := utils.NewUTF8OnlyReader(resp.Body)
	err = xml.NewDecoder(reader).Decode(rssResponse)
	if err != nil {
		return nil, err
	}
	return rssResponse.PackageEvents, nil
}

// Gets the package version & corresponding created date from NPM. Returns
// a slice of {}Package.
func fetchPackage(baseURL, pkgTitle string) ([]*Package, error) {
	versionURL, err := utils.URLPathJoin(baseURL, pkgTitle)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Get(versionURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	err = utils.CheckResponseStatus(resp)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch npm package version data: %w", err)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var jsonMap map[string]interface{}
	err = json.Unmarshal(body, &jsonMap)
	if err != nil {
		return nil, fmt.Errorf("%w : %v for package %s", errJSON, err, pkgTitle)
	}

	// The json string `time` contains versions in date order, oldest to newest.
	versions, ok := jsonMap["time"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%w : 'time' not found for package %s ",
			errJSON, pkgTitle)
	}

	// If `unpublished` exists in the version map then at a given point in time
	// the package was 'entirely' removed, the packageEvent(s) received are for package
	// versions that no longer exist. For a given 24h period no further versions can
	// be uploaded, with any previous versions never being available again.
	// https://www.npmjs.com/policies/unpublish
	_, unPublished := versions["unpublished"]

	if unPublished {
		return nil, fmt.Errorf("%s %w", pkgTitle, errUnpublished)
	}

	// Remove redundant entries in map, we're only interested in actual version pairs.
	delete(versions, "created")
	delete(versions, "modified")

	// Create slice of Package{} to allow sorting of a slice, as maps
	// are unordered.
	versionSlice := []*Package{}
	for version, timestamp := range versions {
		date, err := time.Parse(time.RFC3339, timestamp.(string))
		if err != nil {
			return nil, err
		}
		versionSlice = append(versionSlice,
			&Package{Title: pkgTitle, CreatedDate: date, Version: version})
	}

	// Sort slice of versions into order of most recent.
	sort.SliceStable(versionSlice, func(i, j int) bool {
		return versionSlice[j].CreatedDate.Before(versionSlice[i].CreatedDate)
	})

	return versionSlice, nil
}

func fetchAllPackages(url string) ([]*feeds.Package, []error) {
	pkgs := []*feeds.Package{}
	errs := []error{}
	packageChannel := make(chan []*Package)
	errChannel := make(chan error)
	packageEvents, err := fetchPackageEvents(url)
	if err != nil {
		// If we can't generate package events then return early.
		return pkgs, append(errs, err)
	}
	// Handle the possibility of multiple releases of the same package
	// within the polled `packages` slice.
	uniquePackages := make(map[string]int)
	for _, pkg := range packageEvents {
		uniquePackages[pkg.Title]++
	}

	for pkgTitle, count := range uniquePackages {
		go func(pkgTitle string, count int) {
			pkgs, err := fetchPackage(url, pkgTitle)
			if err != nil {
				if !errors.Is(err, errUnpublished) {
					err = feeds.PackagePollError{Name: pkgTitle, Err: err}
				}
				errChannel <- err
				return
			}
			// Apply count slice
			packageChannel <- pkgs[:count]
		}(pkgTitle, count)
	}

	for i := 0; i < len(uniquePackages); i++ {
		select {
		case npmPkgs := <-packageChannel:
			for _, pkg := range npmPkgs {
				feedPkg := feeds.NewPackage(pkg.CreatedDate, pkg.Title,
					pkg.Version, FeedName)
				pkgs = append(pkgs, feedPkg)
			}
		case err := <-errChannel:
			// When polling the 'firehose' unpublished packages
			// don't need to be logged as an error.
			if !errors.Is(err, errUnpublished) {
				errs = append(errs, err)
			}
		}
	}
	return pkgs, errs
}

func fetchCriticalPackages(url string, packages []string) ([]*feeds.Package, []error) {
	pkgs := []*feeds.Package{}
	errs := []error{}
	packageChannel := make(chan []*Package)
	errChannel := make(chan error)

	for _, pkgTitle := range packages {
		go func(pkgTitle string) {
			pkgs, err := fetchPackage(url, pkgTitle)
			if err != nil {
				if !errors.Is(err, errUnpublished) {
					err = feeds.PackagePollError{Name: pkgTitle, Err: err}
				}
				errChannel <- err
				return
			}
			packageChannel <- pkgs
		}(pkgTitle)
	}

	for i := 0; i < len(packages); i++ {
		select {
		case npmPkgs := <-packageChannel:
			for _, pkg := range npmPkgs {
				feedPkg := feeds.NewPackage(pkg.CreatedDate, pkg.Title,
					pkg.Version, FeedName)
				pkgs = append(pkgs, feedPkg)
			}
		case err := <-errChannel:
			// Assume if a package has been unpublished that it is a valid reason
			// to log the error when polling for 'critical' packages. This could
			// be changed for a 'lossy' type event instead. Further packages should
			// be proccessed.
			errs = append(errs, err)
		}
	}
	return pkgs, errs
}

type Feed struct {
	packages         *[]string
	lossyFeedAlerter *feeds.LossyFeedAlerter
	baseURL          string
	options          feeds.FeedOptions
}

func New(feedOptions feeds.FeedOptions, eventHandler *events.Handler) (*Feed, error) {
	return &Feed{
		packages:         feedOptions.Packages,
		lossyFeedAlerter: feeds.NewLossyFeedAlerter(eventHandler),
		baseURL:          "https://registry.npmjs.org/",
		options:          feedOptions,
	}, nil
}

func (feed Feed) Latest(cutoff time.Time) ([]*feeds.Package, []error) {
	pkgs := []*feeds.Package{}
	var errs []error

	if feed.packages == nil {
		pkgs, errs = fetchAllPackages(feed.baseURL)
	} else {
		pkgs, errs = fetchCriticalPackages(feed.baseURL, *feed.packages)
	}

	if len(pkgs) == 0 {
		// If none of the packages were successfully polled for, return early.
		return nil, append(errs, feeds.ErrNoPackagesPolled)
	}

	// Ensure packages are sorted by CreatedDate in order of most recent, as goroutine
	// concurrency isn't deterministic.
	sort.SliceStable(pkgs, func(i, j int) bool {
		return pkgs[j].CreatedDate.Before(pkgs[i].CreatedDate)
	})

	// TODO: Add an event for checking if the previous package list contains entries
	// that do not exist in the latest package list when polling for critical packages.
	// This can highlight cases where specific versions have been unpublished.
	if feed.packages == nil {
		feed.lossyFeedAlerter.ProcessPackages(FeedName, pkgs)
	}

	pkgs = feeds.ApplyCutoff(pkgs, cutoff)
	return pkgs, errs
}

func (feed Feed) GetName() string {
	return FeedName
}

func (feed Feed) GetFeedOptions() feeds.FeedOptions {
	return feed.options
}
