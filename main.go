package main

import (
	"archive/zip"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fatih/color"
)

//go:embed config.json
var jsonConfig []byte

type ParsedConfig struct {
	Token       string        `json:"token"`
	Users       []string      `json:"users"`
	Repos       []string      `json:"repos"`
	IncludeExts []interface{} `json:"includeExts"`
	ExcludeDirs []string      `json:"excludeDirs"`
}

type Config struct {
	client        *http.Client
	token         string
	usernames     []string
	reponames     []string
	includeExtMap map[string]string
	excludeExtMap map[string]struct{}
}

func main() {
	fmt.Println("Starting the script")
	start := time.Now()

	var config = parseConfig()
	fmt.Printf("Config: %v\n", config)

	config.client = &http.Client{}

	repos := getRepos(config)
	appendExtraRepos(&repos, config.reponames)

	files, reposCount := createFileRepoTables(repos, config)
	tables := createStatTables(files)

	t := time.Now()
	elapsed := t.Sub(start)

	fmt.Println()
	printTable(tables.filesCount, "files")
	printTable(tables.langLinesCount, "lines")
	printTable(tables.linesPerFileAvg, "lines/file")
	printTable(reposCount, "repos")
	printTable(tables.repoLinesCount, "lines")

	for k, v := range tables.groupedLinesCount {
		color.Red("%22s\n", k)
		printTable(v, "lines")
	}

	fmt.Printf("\nScript took %d ms\n", elapsed/time.Millisecond)
}

func parseConfig() Config {
	var parsedConfig ParsedConfig
	err := json.Unmarshal(jsonConfig, &parsedConfig)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}

	config := Config{
		token:         parsedConfig.Token,
		usernames:     parsedConfig.Users,
		reponames:     parsedConfig.Repos,
		excludeExtMap: map[string]struct{}{},
		includeExtMap: map[string]string{},
	}

	for _, dir := range parsedConfig.ExcludeDirs {
		config.excludeExtMap[dir] = struct{}{}
	}

	for _, group := range parsedConfig.IncludeExts {
		switch x := group.(type) {
		case string:
			config.includeExtMap[x] = x
		case []string:
			var groupSb strings.Builder
			for i := 0; i < len(x); i++ {
				groupSb.WriteString(x[i])
				groupSb.WriteString("/")
			}
			groupSb.WriteString(x[len(x)-1])

			for _, elem := range x {
				config.includeExtMap[elem] = groupSb.String()
			}
		}
	}

	return config
}

func getRequest(url string, config Config) []byte {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}
	req.Header.Set("Authorization", "Bearer "+config.token)

	res, err := config.client.Do(req)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}

	if res.StatusCode != 200 {
		fmt.Printf("Non ok request: %d\n", res.StatusCode)
	}
	return body
}

func appendExtraRepos(repos *[]Repo, repoNames []string) {
	for _, repoName := range repoNames {
		var name strings.Builder
		for _, ch := range repoName {
			if ch == '\\' {
				break
			}
			name.WriteRune(ch)
		}
		repo := Repo{
			Name:     repoName,
			Fullname: repoName,
		}
		*repos = append(*repos, repo)
	}
}

func createFileRepoTables(repos []Repo, config Config) ([]FileRecord, map[string]int64) {
	files := make([]FileRecord, 0)
	reposCount := map[string]int64{}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		downloadRepos(repos, &files, config)
		wg.Done()
	}()
	go func() {
		countRepos(repos, reposCount, config)
		wg.Done()
	}()
	wg.Wait()

	return files, reposCount
}

type StatTables struct {
	filesCount        map[string]int64
	langLinesCount    map[string]int64
	linesPerFileAvg   map[string]int64
	repoLinesCount    map[string]int64
	groupedLinesCount map[string]map[string]int64
}

func createStatTables(files []FileRecord) StatTables {
	tables := StatTables{
		filesCount:        map[string]int64{},
		langLinesCount:    map[string]int64{},
		linesPerFileAvg:   map[string]int64{},
		repoLinesCount:    map[string]int64{},
		groupedLinesCount: map[string]map[string]int64{},
	}

	for _, file := range files {
		tables.filesCount[file.Ext] += 1
	}

	for _, file := range files {
		tables.langLinesCount[file.Ext] += file.LinesCount
		tables.repoLinesCount[file.RepoName] += file.LinesCount

		groupLinesCount, ok := tables.groupedLinesCount[file.RepoName]
		if !ok {
			groupLinesCount = map[string]int64{}
			tables.groupedLinesCount[file.RepoName] = groupLinesCount
		}
		groupLinesCount[file.Ext] += file.LinesCount
	}

	for _, file := range files {
		fileCount := tables.filesCount[file.Ext]
		lineCount := tables.langLinesCount[file.Ext]
		tables.linesPerFileAvg[file.Ext] = lineCount / fileCount
	}

	return tables
}

type Repo struct {
	Name     string `json:"name"`
	Fullname string `json:"full_name"`
}

func getRepos(config Config) []Repo {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ch := make(chan struct{}, 10)

	repos := make([]Repo, 0)

	for _, user := range config.usernames {
		wg.Add(1)
		go func(user string) {
			defer wg.Done()

			ch <- struct{}{}
			defer func() {
				<-ch
			}()

			fmt.Printf("Getting repos for user %s\n", user)
			url := fmt.Sprintf("https://api.github.com/users/%s/repos", user)
			body := getRequest(url, config)

			userRepos := make([]Repo, 0)
			err := json.Unmarshal(body, &userRepos)
			if err != nil {
				fmt.Printf("%s\n", err.Error())
				os.Exit(1)
			}

			mu.Lock()
			defer mu.Unlock()
			repos = append(repos, userRepos...)
		}(user)
	}
	wg.Wait()

	return repos
}

func countRepos(repos []Repo, reposCount map[string]int64, config Config) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ch := make(chan struct{}, 10)

	for _, repo := range repos {
		wg.Add(1)
		fmt.Printf("Counting Repo %s\n", repo.Name)

		go func(repo Repo) {
			defer wg.Done()

			ch <- struct{}{}
			defer func() {
				<-ch
			}()

			repoKey := findGreatestLangCount(repo, config)

			mu.Lock()
			defer mu.Unlock()
			reposCount[repoKey] += 1
		}(repo)
	}

	wg.Wait()
}

func findGreatestLangCount(repo Repo, config Config) string {
	url := fmt.Sprintf("https://api.github.com/repos/%s/languages", repo.Fullname)

	body := getRequest(url, config)
	mapRes := map[string]int{}
	err := json.Unmarshal(body, &mapRes)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}

	largestKey, largestValue := "", 0
	for key, val := range mapRes {
		if val > largestValue {
			largestKey = key
			largestValue = val
		}
	}

	return largestKey
}

type FileRecord struct {
	Ext        string
	RepoName   string
	LinesCount int64
}

func downloadRepos(repos []Repo, files *[]FileRecord, config Config) {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ch := make(chan struct{}, 10)

	for _, repo := range repos {
		wg.Add(1)
		fmt.Printf("Download Repo %s\n", repo.Fullname)

		go func(repo Repo) {
			defer wg.Done()

			ch <- struct{}{}
			defer func() {
				<-ch
			}()

			onFile := func(fr FileRecord) {
				mu.Lock()
				defer mu.Unlock()
				*files = append(*files, fr)
			}
			downloadRepo(repo, onFile, config)
		}(repo)
	}

	wg.Wait()
}

func downloadRepo(repo Repo, onFile func(fr FileRecord), config Config) {
	url := fmt.Sprintf("https://github.com/%s/archive/master.zip", repo.Fullname)
	data := getRequest(url, config)

	r := bytes.NewReader(data)
	archive, err := zip.NewReader(r, int64(len(data)))
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}

	for _, zipFile := range archive.File {
		buf := readZipFile(zipFile)

		pathTokens := strings.Split(zipFile.Name, "/")

		// get the extension from the file name
		lastTok := pathTokens[len(pathTokens)-1] // last element is the file extension
		pathTokens = pathTokens[0 : len(pathTokens)-1]
		lastTokTokens := strings.Split(lastTok, ".")

		var ext string
		if len(lastTokTokens) < 1 {
			ext = lastTok
		} else {
			ext = lastTokTokens[len(lastTokTokens)-1]
		}

		// files without extensions are always excluded
		if ext == "" {
			continue
		}

		// skip if any part of the path is contained within the exclude map
		stop := false
		for _, segment := range pathTokens {
			_, exists := config.excludeExtMap[segment]
			if exists {
				stop = true
				break
			}
		}
		if stop {
			continue
		}

		// check if the extension is a grouping type we want to track
		group, ok := config.includeExtMap[ext]
		if ok {
			zfLines := countLines(string(buf))
			record := FileRecord{
				Ext:        group,
				RepoName:   repo.Name,
				LinesCount: zfLines,
			}
			onFile(record)
		}
	}
}

func readZipFile(zf *zip.File) []byte {
	f, err := zf.Open()
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}
	defer f.Close()
	buf, err := io.ReadAll(f)
	if err != nil {
		fmt.Printf("%s\n", err.Error())
		os.Exit(1)
	}
	return buf
}

func countLines(data string) int64 {
	var lineCount int64
	var lineLen int64

	for _, ch := range data {
		if ch == '\r' {
			continue
		}
		if ch == '\n' {
			if lineLen > 0 {
				lineCount++
			}
			lineLen = 0
		}
		if !unicode.IsSpace(ch) {
			lineLen++
		}
	}
	if lineLen > 0 {
		lineCount++
	}

	return lineCount
}

func printTable(m map[string]int64, metric string) {
	type Pair = struct {
		left  string
		right int64
	}

	pairs := make([]Pair, 0)
	var total int64
	maxKeyLen := 22

	for k, v := range m {
		if len(k) > maxKeyLen {
			maxKeyLen = len(k)
		}
		total += v
		pairs = append(pairs, Pair{k, v})
	}

	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].right > pairs[j].right
	})

	for _, pair := range pairs {
		key := pair.left
		if key == "" {
			key = "<none>"
		}
		val := pair.right

		percentage := float64(val) / float64(total) * 100

		_, err := color.New(color.FgCyan).Printf("%*s", maxKeyLen, key)
		if err != nil {
			fmt.Printf("%s\n", err.Error())
			os.Exit(1)
		}
		_, err = color.New(color.FgMagenta).Printf("%10d %s", val, metric)
		if err != nil {
			fmt.Printf("%s\n", err.Error())
			os.Exit(1)
		}

		percentStr := fmt.Sprintf("%.2f", percentage)
		_, err = color.New(color.FgGreen).Printf("%10s%% \t", percentStr)
		if err != nil {
			fmt.Printf("%s\n", err.Error())
			os.Exit(1)
		}
		for i := 0; i < int(percentage); i++ {
			_, err := color.New(color.BgWhite).Printf(" ")
			if err != nil {
				fmt.Printf("%s\n", err.Error())
				os.Exit(1)
			}
		}
		fmt.Println()
	}
	fmt.Println()
}
