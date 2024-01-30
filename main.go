package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"golang.org/x/exp/slices"
)

//go:embed .env
var env string

type Config struct {
	client     *http.Client
	name       string
	token      string
	dirExc     []string
	fileIncMap map[string]string
}

func main() {
	fmt.Println("Starting the script")

	var envMap = getEnvVars(env)
	fmt.Printf("EnvMap: %v\n", envMap)

	config := Config{
		client:     &http.Client{},
		name:       envMap["name"],
		token:      envMap["token"],
		dirExc:     strings.Split(envMap["exclude"], " "),
		fileIncMap: toExtMap(envMap["include"]),
	}

	files := make([]FileRecord, 0)
	reposCount := map[string]int{}
	filesCount := map[string]int{}
	linesCount := map[string]int{}
	repoLCount := map[string]int{}

	repos := getRepos(&config)

	for _, repo := range repos {
		fmt.Println("Repo ", repo.Name)
		baseUrl := fmt.Sprintf("https://api.github.com/repos/%s/%s", config.name, repo.Name)

		countRepo(fmt.Sprintf("%s/languages", baseUrl), reposCount, &config)
		walkRepo(fmt.Sprintf("%s/contents", baseUrl), repo.Name, &files, &config)
	}

	for _, file := range files {
		filesCount[file.Ext] += 1
	}

	countAllLinesWg(linesCount, repoLCount, files, &config)

	fmt.Println()
	printMap(filesCount, "files")
	printMap(linesCount, "lines")
	printMap(reposCount, "repos")
	printMap(repoLCount, "lines")
}

func toExtMap(includeStr string) map[string]string {
	expMap := make(map[string]string)
	for _, group := range strings.Split(includeStr, " ") {
		for _, ext := range strings.Split(group, "/") {
			expMap[ext] = group
		}
	}
	return expMap
}

func getEnvVars(env string) map[string]string {
	envMap := make(map[string]string)
	for _, line := range strings.Split(env, "\n") {
		line := strings.ReplaceAll(line, "\r", "")
		i := strings.Index(line, "=")
		envMap[line[:i]] = line[i+1:]
	}
	return envMap
}

type Repo struct {
	Name string `json:"name"`
}

func countRepo(baseUrl string, reposCount map[string]int, config *Config) {
	body := getRequest(baseUrl, config)
	mapRes := map[string]int{}
	err := json.Unmarshal(body, &mapRes)
	if err != nil {
		onError(err)
	}
	largestK, largestV := "", 0
	for k, v := range mapRes {
		if v > largestV {
			largestK = k
			largestV = v
		}
	}
	reposCount[largestK] += 1
}

func printMap(m map[string]int, metric string) {
	type Pair = struct {
		string
		int
	}

	total := 0
	slice := make([]Pair, 0)
	for k, v := range m {
		total += v
		slice = append(slice, Pair{k, v})
	}
	sort.Slice(slice, func(i, j int) bool {
		return slice[i].int > slice[j].int
	})

	for _, pair := range slice {
		k := pair.string
		if k == "" {
			k = "<none>"
		}
		v := pair.int
		percentage := int(float32(v) / float32(total) * 100)
		fmt.Printf("%18s %8d %s %5d%% \t", k, v, metric, percentage)
		for i := 0; i < percentage; i++ {
			fmt.Print("|")
		}
		fmt.Println()
	}
	fmt.Println()
}

func getRequest(url string, config *Config) []byte {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		onError(err)
	}
	req.Header.Set("Authorization", "Bearer "+config.token)

	// Send the request
	res, err := config.client.Do(req)
	if err != nil {
		onError(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		onError(err)
	}

	if res.StatusCode != 200 {
		fmt.Printf("Non ok request: %d\n", res.StatusCode)
	}
	return body
}

func onError(err error) {
	fmt.Printf("%s\n", err.Error())
	os.Exit(1)
}

func countLines(data string) int {
	count := 0
	for _, line := range strings.Split(data, "\n") {
		if line != "" && line != "\r" {
			count += 1
		}
	}
	return count
}

func getRepos(config *Config) []Repo {
	url := fmt.Sprintf("https://api.github.com/users/%s/repos", config.name)
	body := getRequest(url, config)
	repos := make([]Repo, 0)
	err := json.Unmarshal(body, &repos)
	if err != nil {
		onError(err)
	}

	return repos
}

type FileRecord struct {
	Ext         string
	DownloadUrl string
	RepoName    string
}

func walkRepo(url string, repoName string, files *[]FileRecord, config *Config) {
	type Node struct {
		Name        string `json:"name"`
		DownloadUrl string `json:"download_url"`
	}

	fmt.Println(url)
	body := getRequest(url, config)

	nodes := make([]Node, 0)
	err := json.Unmarshal(body, &nodes)
	if err != nil {
		onError(err)
	}

	for _, node := range nodes {
		tokenized := strings.Split(node.Name, ".")
		ext := tokenized[len(tokenized)-1]
		if node.DownloadUrl != "" {
			// file
			group, ok := config.fileIncMap[ext]
			if ok {
				fmt.Println("File ", node.Name)
				pending := FileRecord{DownloadUrl: node.DownloadUrl, Ext: group, RepoName: repoName}
				*files = append(*files, pending)
			}
		} else {
			// dir
			nextUrl := fmt.Sprintf("%s/%s", url, node.Name)
			if !slices.Contains(config.dirExc, node.Name) {
				walkRepo(nextUrl, repoName, files, config)
			}
		}
	}
}

// deprecated solution with channels
func countAllLinesCh(linesCount map[string]int, files []FileRecord, config *Config) {
	type CountPair struct {
		Ext        string
		LinesCount int
	}

	fmt.Println("Begin download to count lines")

	ch := make(chan CountPair)
	for _, file := range files {
		go func(file FileRecord) {
			fmt.Println("Start ", file.DownloadUrl)
			data := getRequest(file.DownloadUrl, config)
			fmt.Println("Finish ", file.DownloadUrl)

			count := countLines(string(data))
			ch <- CountPair{LinesCount: count, Ext: file.Ext}
		}(file)
	}

	for i := 0; i < len(files); i++ {
		pair := <-ch
		fmt.Println("Channel", pair.Ext, pair.LinesCount)
		linesCount[pair.Ext] += pair.LinesCount
	}
}

func countAllLinesWg(linesCount map[string]int, repoLineCount map[string]int, files []FileRecord, config *Config) {
	fmt.Println("Begin download to count lines")

	var wg sync.WaitGroup
	var m sync.Mutex
	for _, file := range files {
		wg.Add(1)
		go func(file FileRecord) {
			defer wg.Done()

			fmt.Println("Start ", file.DownloadUrl)
			data := getRequest(file.DownloadUrl, config)
			fmt.Println("Finish ", file.DownloadUrl)

			m.Lock()
			defer m.Unlock()

			count := countLines(string(data))
			linesCount[file.Ext] += count
			repoLineCount[file.RepoName] += count
		}(file)
	}

	wg.Wait()
}
