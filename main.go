package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/beevik/etree"
	"github.com/charmbracelet/log"
	"github.com/dustin/go-humanize"
	"github.com/shurcooL/githubv4"
	"golang.org/x/oauth2"
)

var (
	accessToken = os.Getenv("ACCESS_TOKEN")
	userName    = os.Getenv("USER_NAME")
	client      *githubv4.Client
	queryCount  = map[string]int{
		"user_getter":        0,
		"follower_getter":    0,
		"graph_commits":      0,
		"graph_repos_stars":  0,
		"repo_total_commits": 0,
		"recursive_loc":      0,
		"cache_builder":      0,
	}
	ownerID string
	logger  = slog.New(log.NewWithOptions(os.Stderr, log.Options{Level: log.DebugLevel}))
)

func init() {
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: accessToken})
	httpClient := oauth2.NewClient(context.Background(), src)
	client = githubv4.NewClient(httpClient)
}

func queryIncrement(name string) {
	// increment GraphQL call counter
	if _, ok := queryCount[name]; ok {
		queryCount[name]++
	}
}

func plural(n int) string {
	// pluralize words
	if n != 1 {
		return "s"
	}
	return ""
}

// age returns the full age difference between birthdate and today,
// broken down into years, months, and days.
func age(birthdate, today time.Time) (years, months, days int) {
	// Normalize both dates to the same location and zero out the time portion
	today = today.In(birthdate.Location())
	y, m, d := today.Date()
	today = time.Date(y, m, d, 0, 0, 0, 0, birthdate.Location())

	by, bm, bd := birthdate.Date()
	birthdate = time.Date(by, bm, bd, 0, 0, 0, 0, birthdate.Location())

	// If birthdate is in the future, return zeros
	if today.Before(birthdate) {
		return 0, 0, 0
	}

	// Initial year, month, day differences
	years = y - by
	months = int(m - bm)
	days = d - bd

	// Adjust days and months if needed
	if days < 0 {
		// Borrow days from the previous month
		prevMonth := today.AddDate(0, -1, 0)
		_, pm, _ := prevMonth.Date()
		// Get the number of days in that previous month
		daysInPrevMonth := time.Date(prevMonth.Year(), pm+1, 0, 0, 0, 0, 0, birthdate.Location()).Day()
		days += daysInPrevMonth
		months--
	}

	if months < 0 {
		months += 12
		years--
	}

	return years, months, days
}

// dailyReadme returns the age string since birthday
func dailyReadme(birthday time.Time) string {
	today := time.Now()
	y, mo, d := age(birthday, today)

	return fmt.Sprintf(
		"%d year%s, %d month%s, %d day%s",
		y, plural(y),
		mo, plural(mo),
		d, plural(d),
	)
}

// loadBirthdayFromEnv reads environment variable envKey as YYYY-MM-DD and returns time.Time
func loadBirthdayFromEnv(envKey string) (time.Time, error) {
	dobStr := os.Getenv(envKey)
	if dobStr == "" {
		return time.Time{}, fmt.Errorf("environment variable %s is not set (expected YYYY-MM-DD)", envKey)
	}
	birthday, err := time.Parse("2006-01-02", dobStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date format for %s: %w", envKey, err)
	}
	return birthday, nil
}

// CacheEntry represents one repo's cached data
type CacheEntry struct {
	Hash        string
	CommitCount int // total commits
	MyCommits   int // commits by user
	Additions   int
	Deletions   int
}

const commentSize = 7

func cacheFile() string {
	// path for cache file
	h := sha256.Sum256([]byte(userName))
	return filepath.Join("cache", hex.EncodeToString(h[:])+".txt")
}

// loadCache reads comment lines and cache entries
func loadCache() ([]string, []CacheEntry, error) {
	path := cacheFile()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// initialize empty cache
		comments := make([]string, commentSize)
		for i := range comments {
			comments[i] = "# comment\n"
		}
		return comments, nil, nil
	} else if err != nil {
		return nil, nil, err
	}
	lines := strings.Split(string(data), "\n")
	comments := lines[:commentSize]
	raw := lines[commentSize:]
	entries := make([]CacheEntry, 0, len(raw))
	for _, line := range raw {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		n0, _ := strconv.Atoi(f[1])
		n1, _ := strconv.Atoi(f[2])
		n2, _ := strconv.Atoi(f[3])
		n3, _ := strconv.Atoi(f[4])
		entries = append(entries, CacheEntry{f[0], n0, n1, n2, n3})
	}
	return comments, entries, nil
}

// saveCache writes comments and entries
func saveCache(comments []string, entries []CacheEntry) error {
	if err := os.MkdirAll("cache", 0755); err != nil {
		return err
	}
	lines := slices.Clone(comments)
	for _, e := range entries {
		lines = append(lines,
			fmt.Sprintf("%s %d %d %d %d", e.Hash, e.CommitCount, e.MyCommits, e.Additions, e.Deletions),
		)
	}
	return os.WriteFile(cacheFile(), []byte(strings.Join(lines, "\n")), 0644)
}

// userGetter returns GitHub user ID and account creation time
func userGetter(login string) (string, time.Time, error) {
	queryIncrement("user_getter")
	var q struct {
		User struct {
			ID        githubv4.ID
			CreatedAt githubv4.DateTime
		} `graphql:"user(login: $login)"`
	}
	vars := map[string]any{"login": githubv4.String(login)}
	if err := client.Query(context.Background(), &q, vars); err != nil {
		return "", time.Time{}, err
	}
	return q.User.ID.(string), q.User.CreatedAt.Time, nil
}

// followerGetter returns follower count
func followerGetter(login string) (int, error) {
	queryIncrement("follower_getter")
	var q struct {
		User struct {
			Followers struct{ TotalCount githubv4.Int }
		} `graphql:"user(login: $login)"`
	}
	vars := map[string]any{"login": githubv4.String(login)}
	if err := client.Query(context.Background(), &q, vars); err != nil {
		return 0, err
	}
	return int(q.User.Followers.TotalCount), nil
}

// graphCommits counts total contributions between dates
func graphCommits(start, end time.Time) (int, error) {
	queryIncrement("graph_commits")
	if start.IsZero() {
		start = end.AddDate(-1, 0, 0)
	}
	if end.Before(start) {
		return 0, nil
	}
	total, curr := 0, start
	for curr.Before(end) {
		next := curr.AddDate(1, 0, 0)
		if next.After(end) {
			next = end
		}
		var q struct {
			User struct {
				ContributionsCollection struct {
					ContributionCalendar struct{ TotalContributions githubv4.Int } `graphql:"contributionCalendar"`
				} `graphql:"contributionsCollection(from: $from, to: $to)"`
			} `graphql:"user(login: $login)"`
		}
		vars := map[string]any{
			"login": githubv4.String(userName),
			"from":  githubv4.DateTime{Time: curr},
			"to":    githubv4.DateTime{Time: next},
		}
		if err := client.Query(context.Background(), &q, vars); err != nil {
			return 0, err
		}
		total += int(q.User.ContributionsCollection.ContributionCalendar.TotalContributions)
		curr = next
	}
	return total, nil
}

// graphReposStars returns repo and star count
func graphReposStars(affs []githubv4.RepositoryAffiliation) (int, int, error) {
	queryIncrement("graph_repos_stars")
	var totalStars, reposCount int
	var cursor *githubv4.String
	for {
		var q struct {
			User struct {
				Repositories struct {
					TotalCount githubv4.Int
					Edges      []struct {
						Node struct {
							Stargazers struct{ TotalCount githubv4.Int }
						}
					} `graphql:"edges"`
					PageInfo struct {
						HasNextPage githubv4.Boolean
						EndCursor   githubv4.String
					} `graphql:"pageInfo"`
				} `graphql:"repositories(first:100, after: $cursor, ownerAffiliations: $affs)"`
			} `graphql:"user(login: $login)"`
		}
		vars := map[string]any{"login": githubv4.String(userName), "affs": affs, "cursor": cursor}
		if err := client.Query(context.Background(), &q, vars); err != nil {
			return 0, 0, err
		}
		reposCount = int(q.User.Repositories.TotalCount)
		for _, e := range q.User.Repositories.Edges {
			totalStars += int(e.Node.Stargazers.TotalCount)
		}
		if !bool(q.User.Repositories.PageInfo.HasNextPage) {
			break
		}
		cursor = &q.User.Repositories.PageInfo.EndCursor
	}
	return reposCount, totalStars, nil
}

// repoTotalCommits fetches total commits for a repository (all authors)
func repoTotalCommits(owner, repo string) (int, error) {
	queryIncrement("repo_total_commits")
	var q struct {
		Repository struct {
			DefaultBranchRef struct {
				Target struct {
					Commit struct {
						History struct{ TotalCount githubv4.Int } `graphql:"history"`
					} `graphql:"... on Commit"`
				} `graphql:"target"`
			} `graphql:"defaultBranchRef"`
		} `graphql:"repository(owner: $owner, name: $repo)"`
	}
	vars := map[string]any{"owner": githubv4.String(owner), "repo": githubv4.String(repo)}
	if err := client.Query(context.Background(), &q, vars); err != nil {
		return 0, err
	}
	return int(q.Repository.DefaultBranchRef.Target.Commit.History.TotalCount), nil
}

// recursiveLocDetail pages user-only commit history to sum additions/deletions
func recursiveLocDetail(owner, repo string) (int, int, int, error) {
	queryIncrement("recursive_loc")
	var cursor *githubv4.String
	adds, dels, myCount := 0, 0, 0
	for {
		var q struct {
			Repository struct {
				DefaultBranchRef struct {
					Target struct {
						Commit struct {
							History struct {
								TotalCount githubv4.Int
								Edges      []struct {
									Node struct {
										Additions int `graphql:"additions"`
										Deletions int `graphql:"deletions"`
									}
								} `graphql:"edges"`
								PageInfo struct {
									HasNextPage githubv4.Boolean
									EndCursor   githubv4.String
								} `graphql:"pageInfo"`
							} `graphql:"history(first:100, after: $cursor, author: $author)"`
						} `graphql:"... on Commit"`
					} `graphql:"target"`
				} `graphql:"defaultBranchRef"`
			} `graphql:"repository(owner: $owner, name: $repo)"`
		}
		vars := map[string]any{
			"owner":  githubv4.String(owner),
			"repo":   githubv4.String(repo),
			"cursor": cursor,
			"author": githubv4.CommitAuthor{ID: githubv4.NewID(ownerID)},
		}
		if err := client.Query(context.Background(), &q, vars); err != nil {
			return 0, 0, 0, err
		}
		h := q.Repository.DefaultBranchRef.Target.Commit.History
		myCount = int(h.TotalCount)
		for _, edge := range h.Edges {
			adds += edge.Node.Additions
			dels += edge.Node.Deletions
		}
		if !bool(h.PageInfo.HasNextPage) {
			break
		}
		cursor = &h.PageInfo.EndCursor
	}
	return myCount, adds, dels, nil
}

// cacheBuilder updates or creates cache using separate total and filtered queries
func cacheBuilder(affs []githubv4.RepositoryAffiliation, force bool) (int, int, int, bool, error) {
	queryIncrement("cache_builder")

	// 1) Load old cache into a map
	comments, oldEntries, err := loadCache()
	if err != nil {
		return 0, 0, 0, false, err
	}
	oldMap := make(map[string]CacheEntry, len(oldEntries))
	for _, e := range oldEntries {
		oldMap[e.Hash] = e
	}

	// 2) Fetch current repo list
	all := []string{}
	var cursor *githubv4.String
	for {
		var q struct {
			User struct {
				Repositories struct {
					Edges []struct {
						Node struct{ NameWithOwner githubv4.String }
					} `graphql:"edges"`
					PageInfo struct {
						HasNextPage githubv4.Boolean
						EndCursor   githubv4.String
					} `graphql:"pageInfo"`
				} `graphql:"repositories(first:60, after: $cursor, ownerAffiliations: $affs)"`
			} `graphql:"user(login: $login)"`
		}
		vars := map[string]any{
			"login":  githubv4.String(userName),
			"affs":   affs,
			"cursor": cursor,
		}
		if err := client.Query(context.Background(), &q, vars); err != nil {
			return 0, 0, 0, false, err
		}
		for _, e := range q.User.Repositories.Edges {
			all = append(all, string(e.Node.NameWithOwner))
		}
		if !bool(q.User.Repositories.PageInfo.HasNextPage) {
			break
		}
		cursor = &q.User.Repositories.PageInfo.EndCursor
	}

	// 3) Build new entries in the same order
	newEntries := make([]CacheEntry, 0, len(all))
	hashToRepo := make(map[string]string, len(all))
	totalAdd, totalDel := 0, 0

	for _, repo := range all {
		// Compute hash and map back to repo name
		h := fmt.Sprintf("%x", sha256.Sum256([]byte(repo)))
		hashToRepo[h] = repo

		// Always re-fetch the global total‐commit count
		parts := strings.Split(repo, "/")
		totalCommits, err := repoTotalCommits(parts[0], parts[1])
		if err != nil {
			return 0, 0, 0, false, err
		}

		old, found := oldMap[h]
		var entry CacheEntry

		// Decide if we need a full LoC recount
		if force || !found || totalCommits != old.CommitCount {
			myCount, adds, dels, err := recursiveLocDetail(parts[0], parts[1])
			if err != nil {
				return 0, 0, 0, false, err
			}
			entry = CacheEntry{
				Hash:        h,
				CommitCount: totalCommits,
				MyCommits:   myCount,
				Additions:   adds,
				Deletions:   dels,
			}
		} else {
			entry = old
		}

		newEntries = append(newEntries, entry)
		totalAdd += entry.Additions
		totalDel += entry.Deletions
	}

	// 4) Recap what changed
	var newRepos, deletedRepos, changedRepos []string
	sumAddChange, sumDelChange := 0, 0

	// Detect new repos & changed‐commit repos, accumulate LoC diffs
	for _, ne := range newEntries {
		if old, ok := oldMap[ne.Hash]; !ok {
			newRepos = append(newRepos, hashToRepo[ne.Hash])
		} else if old.CommitCount != ne.CommitCount {
			changedRepos = append(changedRepos,
				fmt.Sprintf("%s (%d→%d)", hashToRepo[ne.Hash], old.CommitCount, ne.CommitCount),
			)
			sumAddChange += ne.Additions - old.Additions
			sumDelChange += ne.Deletions - old.Deletions
		}
	}
	// Detect deleted repos
	for h := range oldMap {
		if _, ok := hashToRepo[h]; !ok {
			deletedRepos = append(deletedRepos, h)
		}
	}

	// Log each category separately
	if len(newRepos) > 0 {
		logger.Info("new repos", "repos", newRepos)
	}
	if len(deletedRepos) > 0 {
		logger.Info("deleted repos", "hashes", deletedRepos)
	}
	if len(changedRepos) > 0 {
		logger.Info("repos with changed commits",
			"repos", changedRepos,
			"lines_added", sumAddChange,
			"lines_removed", sumDelChange,
		)
	}

	// 5) Persist and return
	net := totalAdd - totalDel
	if err := saveCache(comments, newEntries); err != nil {
		return totalAdd, totalDel, net, false, err
	}
	return totalAdd, totalDel, net, len(all) == len(oldEntries) && !force, nil
}

// justifyFormat updates SVG text and its preceding dots to align to `length`
func justifyFormat(doc *etree.Document, elementID, newText string, length int) {
	// replace text
	if el := doc.FindElement(fmt.Sprintf("//*[@id='%s']", elementID)); el != nil {
		el.SetText(newText)
	}
	// only adjust dots if length > 0
	if length > 0 {
		justLen := length - len(newText)
		var dotString string
		if justLen <= 2 && elementID != "repo_data" {
			dotMap := map[int]string{0: "", 1: " ", 2: ". "}
			dotString = dotMap[justLen]
		} else {
			dotString = " " + strings.Repeat(".", justLen) + " "
		}
		// replace dots element
		if el := doc.FindElement(fmt.Sprintf("//*[@id='%s_dots']", elementID)); el != nil {
			el.SetText(dotString)
		}
	}
}

// svgOverwrite updates SVG text elements and justifies them
func svgOverwrite(filename string, elements map[string]string) error {
	doc := etree.NewDocument()
	if err := doc.ReadFromFile(filename); err != nil {
		return err
	}
	// update raw elements
	for id, text := range elements {
		if el := doc.FindElement(fmt.Sprintf("//*[@id='%s']", id)); el != nil {
			el.SetText(text)
		}
	}
	// apply justification (lengths match Python version)
	justifyFormat(doc, "age_data", elements["age_data"], 49)
	justifyFormat(doc, "commit_data", elements["commit_data"], 22)
	justifyFormat(doc, "star_data", elements["star_data"], 14)
	justifyFormat(doc, "repo_data", elements["repo_data"], 7-len(elements["contrib_data"]))
	justifyFormat(doc, "contrib_data", elements["contrib_data"], 0)
	justifyFormat(doc, "follower_data", elements["follower_data"], 10)
	justifyFormat(doc, "loc_data", elements["loc_data"], 9)
	justifyFormat(doc, "loc_add", elements["loc_add"], 0)
	justifyFormat(doc, "loc_del", elements["loc_del"], 7)
	// write back
	return doc.WriteToFile(filename)
}

func main() {
	if accessToken == "" {
		logger.Error("missing required environment variable", "env", "ACCESS_TOKEN")
		os.Exit(1)
	}
	if userName == "" {
		logger.Error("missing required environment variable", "env", "USER_NAME")
		os.Exit(1)
	}

	// ensure DATE_OF_BIRTH is set and valid (YYYY-MM-DD)
	birthday, err := loadBirthdayFromEnv("DATE_OF_BIRTH")
	if err != nil {
		logger.Error("missing or invalid environment variable", "env", "DATE_OF_BIRTH", "error", err)
		os.Exit(1)
	}

	now := time.Now()
	zone, offset := now.Zone()

	slog.Info("current_time",
		"time", now.String(),
		"timestamp", now.Format(time.RFC3339),
		"zone", zone,
		"offset_sec", offset,
	)

	// userGetter
	start := time.Now()
	id, createdAt, err := userGetter(userName)
	if err != nil {
		logger.Error("userGetter", "error", err)
		os.Exit(1)
	}
	ownerID = id
	logger.Info("calculation_time",
		"phase", "account_data",
		"duration_s", time.Since(start).Seconds(),
	)

	// age
	start = time.Now()
	ageStr := dailyReadme(birthday)
	logger.Info("calculation_time",
		"phase", "age_calculation",
		"duration_s", time.Since(start).Seconds(),
	)

	// commit graph
	start = time.Now()
	commitCount, err := graphCommits(createdAt, time.Now())
	if err != nil {
		logger.Error("graphCommits", "error", err)
	}
	logger.Info("calculation_time",
		"phase", "graph_commits",
		"duration_s", time.Since(start).Seconds(),
	)

	// repos & stars
	start = time.Now()
	repos, stars, err := graphReposStars([]githubv4.RepositoryAffiliation{githubv4.RepositoryAffiliationOwner})
	if err != nil {
		logger.Error("graphReposStars owner", "error", err)
	}
	logger.Info("calculation_time",
		"phase", "repos_and_stars",
		"duration_s", time.Since(start).Seconds(),
	)

	// cache builder
	start = time.Now()
	add, del, net, cached, err := cacheBuilder([]githubv4.RepositoryAffiliation{
		githubv4.RepositoryAffiliationOwner,
		githubv4.RepositoryAffiliationCollaborator,
		githubv4.RepositoryAffiliationOrganizationMember,
	}, false)
	if err != nil {
		logger.Error("cacheBuilder", "error", err)
	}
	var isCached string
	if cached {
		isCached = "true"
	} else {
		isCached = "false"
	}
	logger.Info("calculation_time",
		"phase", "loc_cache_builder",
		"cached", isCached,
		"duration_s", time.Since(start).Seconds(),
	)

	// followers
	start = time.Now()
	followers, _ := followerGetter(userName)
	logger.Info("calculation_time",
		"phase", "follower_count",
		"duration_s", time.Since(start).Seconds(),
	)

	// total time
	total := 0.0
	for _, v := range queryCount {
		total += float64(v)
	}
	logger.Info("total_graphql_calls",
		"count", total,
	)

	// write SVGs
	elements := map[string]string{
		"age_data":      ageStr,
		"commit_data":   strconv.Itoa(commitCount),
		"star_data":     strconv.Itoa(stars),
		"repo_data":     strconv.Itoa(repos),
		"contrib_data":  strconv.Itoa(commitCount),
		"loc_data":      humanize.Comma(int64(net)),
		"loc_add":       humanize.Comma(int64(add)),
		"loc_del":       humanize.Comma(int64(del)),
		"follower_data": strconv.Itoa(followers),
	}
	err = svgOverwrite("dark_mode.svg", elements)
	if err != nil {
		logger.Error("svgOverwrite", "filename", "dark_mode.svg", "error", err)
	}
	err = svgOverwrite("light_mode.svg", elements)
	if err != nil {
		logger.Error("svgOverwrite", "filename", "light_mode.svg", "error", err)
	}
}
