// Package gocovmerge takes the results from multiple `go test -coverprofile`
// runs and merges them into one profile
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/cover"
)

var (
	g_strOutCoverFile = flag.String("outcover", "cover.txt", "输出覆盖率文件")
	g_strOutHTMLFile  = flag.String("outhtml", "cover.html", "输出覆盖率HTML文件")
)

func main() {
	// 自定义帮助信息
	flag.Usage = func() {
		fmt.Println("Usage: ./bin/gocovmerge [options] [cover.txt.timestamp.hash cover.txt.1723042827.e24dac6 ...]")
		fmt.Println("Options:")
		flag.PrintDefaults() // 打印默认的参数帮助信息
	}

	flag.Parse()
	coverFiles := flag.Args()
	if len(coverFiles) == 0 {
		fmt.Println("Error: cover.txt.xxx.xxx file required.")
		flag.Usage()
		os.Exit(1)
	}

	if err := run(coverFiles); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	fmt.Println("generate ", *g_strOutCoverFile, " and ", *g_strOutHTMLFile, " ok.")
}

func run(coverFiles []string) error {
	mapCoverFiles := make(map[string][]*CoverFileInfo) // githas -> file -> info
	for _, file := range coverFiles {
		fileInfo, err := ParseCoverFileInfo(file)
		if err != nil {
			return fmt.Errorf("failed to parse version profiles: %v", err)
		}
		if _, ok := mapCoverFiles[fileInfo.GitHash]; !ok {
			mapCoverFiles[fileInfo.GitHash] = make([]*CoverFileInfo, 0)
		}
		mapCoverFiles[fileInfo.GitHash] = append(mapCoverFiles[fileInfo.GitHash], fileInfo)
	}

	// 遍历 mapCoverFiles 并按时间排序每个切片
	for _, coverFiles := range mapCoverFiles {
		sort.Slice(coverFiles, func(i, j int) bool {
			return coverFiles[i].Timestamp < coverFiles[j].Timestamp
		})
	}

	var mergedCoverFiles []*CoverFileInfo
	for gitHash, coverFiles := range mapCoverFiles {
		var merged []*cover.Profile
		for _, coverFile := range coverFiles {
			profiles, err := cover.ParseProfiles(coverFile.FileName)
			if err != nil {
				return fmt.Errorf("failed to parse profiles: %v", err)
			}
			for _, p := range profiles {
				merged = AddProfile(merged, p)
			}
		}
		fileInfo := &CoverFileInfo{
			GitHash:   gitHash,
			Timestamp: coverFiles[0].Timestamp,
			FileName:  "",
			Profiles:  merged,
		}
		mergedCoverFiles = append(mergedCoverFiles, fileInfo)
	}

	// 遍历 mergedCoverFiles 并按时间排序
	sort.Slice(mergedCoverFiles, func(i, j int) bool {
		return mergedCoverFiles[i].Timestamp < mergedCoverFiles[j].Timestamp
	})

	// 根据版本号对比文件内容，相同的合并，不同的分开文件
	mergedByHash := make(map[string][]*cover.Profile)
	// 双层循环比较 i 和 j (i < j)
	for i := 0; i < len(mergedCoverFiles); i++ {
		currentCoverFile := mergedCoverFiles[i]
		for _, p := range currentCoverFile.Profiles {
			mergedByHash[currentCoverFile.GitHash] = AddProfile(mergedByHash[currentCoverFile.GitHash], p)
		}
		for j := i + 1; j < len(mergedCoverFiles); j++ {
			nextCoverFile := mergedCoverFiles[j]
			var newProfiles []*cover.Profile
			for _, p := range nextCoverFile.Profiles {
				filePath := fmt.Sprintf("go/src/%s", p.FileName)
				bSame, _ := CompareVersions(currentCoverFile.GitHash, nextCoverFile.GitHash, filePath)
				if bSame {
					mergedByHash[currentCoverFile.GitHash] = AddProfile(mergedByHash[currentCoverFile.GitHash], p)
				} else {
					newProfiles = append(newProfiles, p)
				}
			}
			mergedCoverFiles[j] = &CoverFileInfo{
				GitHash:   nextCoverFile.GitHash,
				Timestamp: nextCoverFile.Timestamp,
				FileName:  "",
				Profiles:  newProfiles,
			}
		}
	}

	// 给文件名加上 git hash, 再合并
	var merged []*cover.Profile
	delFiles := make([]string, 0)
	for gitHash, profiles := range mergedByHash {
		for _, p := range profiles {
			filePath := fmt.Sprintf("go/src/%s", p.FileName)
			outputPath := fmt.Sprintf("go/src/%s.%s", p.FileName, gitHash)
			delFiles = append(delFiles, outputPath)
			err := GitSaveFile(gitHash, filePath, outputPath)
			if err != nil {
				return err
			}
			p.FileName = fmt.Sprintf("%s.%s", p.FileName, gitHash)

			// 合并
			for _, p := range profiles {
				merged = AddProfile(merged, p)
			}
		}
	}
	defer DeleteFiles(delFiles)

	outFile, err := os.Create(*g_strOutCoverFile)
	if err != nil {
		fmt.Errorf("Error creating outFile: %v", err)
		return err
	}
	defer outFile.Close()

	err = DumpProfiles(merged, outFile)
	if err != nil {
		return err
	}
	return GenerateCoverHTML(*g_strOutCoverFile, *g_strOutHTMLFile)
}

// 从 cover.txt 生成 HTML 报告
func GenerateCoverHTML(coverFile string, outputFile string) error {
	// 获取当前工作目录
	currDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get current working directory: %w", err)
	}

	// 构造命令
	cmd := exec.Command("go", "tool", "cover", fmt.Sprintf("-html=%s", coverFile), "-o", outputFile)

	// 设置 GOPATH 环境变量（局部）
	cmd.Env = append(os.Environ(), fmt.Sprintf("GOPATH=%s/go", currDir))

	// 将标准输出和标准错误设置为主进程的输出
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// 运行命令
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("error executing command: %w", err)
	}

	// 处理 HTML 文件结果
	return InsertAdditionHTML(outputFile)
}

func AddProfile(profiles []*cover.Profile, p *cover.Profile) []*cover.Profile {
	i := sort.Search(len(profiles), func(i int) bool { return profiles[i].FileName >= p.FileName })
	if i < len(profiles) && profiles[i].FileName == p.FileName {
		MergeProfiles(profiles[i], p)
	} else {
		profiles = append(profiles, nil)
		copy(profiles[i+1:], profiles[i:])
		profiles[i] = p
	}
	return profiles
}

func DumpProfiles(profiles []*cover.Profile, out io.Writer) error {
	if len(profiles) == 0 {
		return nil
	}
	if _, err := fmt.Fprintf(out, "mode: %s\n", profiles[0].Mode); err != nil {
		return err
	}
	for _, p := range profiles {
		for _, b := range p.Blocks {
			if _, err := fmt.Fprintf(out, "%s:%d.%d,%d.%d %d %d\n", p.FileName, b.StartLine, b.StartCol, b.EndLine, b.EndCol, b.NumStmt, b.Count); err != nil {
				return err
			}
		}
	}
	return nil
}

func MergeProfiles(into *cover.Profile, merge *cover.Profile) error {
	if into.Mode != merge.Mode {
		return fmt.Errorf("cannot merge profiles with different modes")
	}
	// Since the blocks are sorted, we can keep track of where the last block
	// was inserted and only look at the blocks after that as targets for merge
	startIndex := 0
	for _, b := range merge.Blocks {
		var err error
		startIndex, err = mergeProfileBlock(into, b, startIndex)
		if err != nil {
			return err
		}
	}
	return nil
}

func mergeProfileBlock(p *cover.Profile, pb cover.ProfileBlock, startIndex int) (int, error) {
	sortFunc := func(i int) bool {
		pi := p.Blocks[i+startIndex]
		return pi.StartLine >= pb.StartLine && (pi.StartLine != pb.StartLine || pi.StartCol >= pb.StartCol)
	}

	i := 0
	if sortFunc(i) != true {
		i = sort.Search(len(p.Blocks)-startIndex, sortFunc)
	}

	i += startIndex
	if i < len(p.Blocks) && p.Blocks[i].StartLine == pb.StartLine && p.Blocks[i].StartCol == pb.StartCol {
		if p.Blocks[i].EndLine != pb.EndLine || p.Blocks[i].EndCol != pb.EndCol {
			return i, fmt.Errorf("gocovmerge: overlapping merge %v %v %v", p.FileName, p.Blocks[i], pb)
		}
		switch p.Mode {
		case "set":
			p.Blocks[i].Count |= pb.Count
		case "count", "atomic":
			p.Blocks[i].Count += pb.Count
		default:
			return i, fmt.Errorf("gocovmerge: unsupported covermode '%s'", p.Mode)
		}

	} else {
		if i > 0 {
			pa := p.Blocks[i-1]
			if pa.EndLine >= pb.EndLine && (pa.EndLine != pb.EndLine || pa.EndCol > pb.EndCol) {
				return i, fmt.Errorf("gocovmerge: overlap before %v %v %v", p.FileName, pa, pb)
			}
		}
		if i < len(p.Blocks)-1 {
			pa := p.Blocks[i+1]
			if pa.StartLine <= pb.StartLine && (pa.StartLine != pb.StartLine || pa.StartCol < pb.StartCol) {
				return i, fmt.Errorf("gocovmerge: overlap after %v %v %v", p.FileName, pa, pb)
			}
		}
		p.Blocks = append(p.Blocks, cover.ProfileBlock{})
		copy(p.Blocks[i+1:], p.Blocks[i:])
		p.Blocks[i] = pb
	}

	return i + 1, nil
}

type CoverFileInfo struct {
	Timestamp int64
	GitHash   string
	FileName  string
	Profiles  []*cover.Profile
}

func ParseCoverFileInfo(fileName string) (*CoverFileInfo, error) {
	// 使用字符串分割
	parts := strings.Split(fileName, ".")
	if len(parts) < 2 {
		return &CoverFileInfo{}, fmt.Errorf("file string is not valid")
	}

	// 倒数第二个是时间戳
	timestampStr := parts[len(parts)-2]
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return &CoverFileInfo{}, fmt.Errorf("timestamp is not valid")
	}
	// 最后一个是git hash
	gitHash := parts[len(parts)-1]

	return &CoverFileInfo{
		Timestamp: timestamp,
		GitHash:   gitHash,
		FileName:  fileName,
	}, nil
}

// 获取指定版本的文件内容
func GitGetFileContent(commit, filePath string) (string, error) {
	cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", commit, filePath))
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "", err
	}
	return out.String(), nil
}

// 比较两个版本的文件内容
func CompareVersions(commit1, commit2, filePath string) (bool, error) {
	content1, err := GitGetFileContent(commit1, filePath)
	if err != nil {
		return false, fmt.Errorf("获取 %s:%s 版本文件失败: %v", commit1, filePath, err)
	}

	content2, err := GitGetFileContent(commit2, filePath)
	if err != nil {
		return false, fmt.Errorf("获取 %s:%s 版本文件失败: %v", commit2, filePath, err)
	}

	return content1 == content2, nil
}

// 检出指定提交中的文件并重命名
func GitSaveFile(commit string, filePath string, outputPath string) error {
	// 创建一个临时文件获取 git show 的输出
	cmd := exec.Command("git", "show", fmt.Sprintf("%s:%s", commit, filePath))
	output, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("failed to run git show: %w", err)
	}

	// 确保保存文件的目录存在
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, os.ModePerm); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	// 将输出写入指定文件
	if err := ioutil.WriteFile(outputPath, output, 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	return nil
}

// 删除给定路径切片中的所有文件
func DeleteFiles(filePaths []string) {
	for _, filePath := range filePaths {
		err := os.Remove(filePath)
		if err != nil {
			// 返回详细错误信息，包括出错的文件路径
			fmt.Errorf("failed to delete file %s: %w", filePath, err)
		}
	}
}

// 插入 HTML 代码:添加文件列表搜索框，添加行号
var g_additionHTML = `
    <style>
        .line-number {
            display: inline-block;
            width: 30px;
            text-align: right;
            margin-right: 10px;
            color: #888;
        }
    </style>
    <script>
    let optionMap = new Map();

    function initFilter() {
        var fileSelect = document.getElementById('files');
        var options = fileSelect.getElementsByTagName('option');

        for (var i = 0; i < options.length; i++) {
            let value = options[i].value;
            optionMap.set(value, options[i]);
        }
    }

    function filterFiles() {
        var input = document.getElementById('fileSearch');
        var filter = input.value.trim().toUpperCase().replace(/_/g, '\_'); // 添加替换下划线的部分
        var visibleOptions = [];

        optionMap.forEach((option, value) => {
            const optionText = option.innerText.toUpperCase().replace(/_/g, '\_'); // 对选项文本也做相同处理
            if (filter === '' || optionText.indexOf(filter) !== -1) {
                visibleOptions.push(option);
            } else {
                option.style.display = 'none';
            }
        });

        for (let option of visibleOptions) {
            option.style.display = '';
        }
    }

    function addLineNumbers() {
      const preElements = document.querySelectorAll('pre');
      preElements.forEach(pre => {
          const lines = pre.innerHTML.split('\n');
          const lineNumberedHtml = lines.map((line, index) => {
              let num = index + 1;
              return '<span class="line-number">'+num+'</span>'+line;
          }).join('\n');
          pre.innerHTML = lineNumberedHtml;
          pre.style.whiteSpace = 'pre';
      });
    }

    // 在页面加载完成后初始化过滤器
    window.onload = function () {
        initFilter();
        addLineNumbers();
    };
    </script>

    <input id="fileSearch" type="text" onkeyup="filterFiles()" placeholder="Search files...">
`

// 从指定的 HTML 文件中读取内容，插入 HTML 代码，然后覆盖写入文件
func InsertAdditionHTML(filePath string) error {
	// 读取原始 HTML 文件
	htmlContent, err := ioutil.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("error reading file: %v", err)
	}

	// 将读取的内容转换为字符串
	htmlString := string(htmlContent)

	// 检查搜索框 HTML 是否已经存在
	existingSearchBoxRe := regexp.MustCompile(`(<input\s+id="fileSearch".*?>)`)
	if existingSearchBoxRe.MatchString(htmlString) {
		// 如果存在，则无需进行替换
		fmt.Println("Search box already exists in the HTML file.")
		return nil
	}

	// 使用正则表达式进行替换
	re := regexp.MustCompile(`(<select id="files">)`)
	htmlString = re.ReplaceAllString(htmlString, g_additionHTML+`$1`)

	// 写回到同一个 HTML 文件
	err = ioutil.WriteFile(filePath, []byte(htmlString), 0644)
	if err != nil {
		return fmt.Errorf("error writing file: %v", err)
	}

	return nil
}
