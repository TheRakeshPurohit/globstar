package analysis

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
)

func verifyIssues(expectedIssues, raisedIssues *map[string]map[int][]string) string {
	var diffBuilder strings.Builder

	// Compare files
	for filePath, expectedFileIssues := range *expectedIssues {
		raisedFileIssues, exists := (*raisedIssues)[filePath]
		if !exists {
			diffBuilder.WriteString(fmt.Sprintf("\nFile: %s\n", filePath))
			diffBuilder.WriteString("  Expected issues but found none\n")
			continue
		}

		// Compare line numbers in each file
		for line, expectedMessages := range expectedFileIssues {
			raisedMessages, exists := raisedFileIssues[line]
			if !exists {
				diffBuilder.WriteString(fmt.Sprintf("\nFile: %s, Line: %d\n", filePath, line))
				diffBuilder.WriteString("  Expected:\n")
				for _, msg := range expectedMessages {
					diffBuilder.WriteString(fmt.Sprintf("    - %s\n", msg))
				}
				diffBuilder.WriteString("  Got: no issues\n")
				continue
			}

			// Compare messages at each line
			if !messagesEqual(expectedMessages, raisedMessages) {
				diffBuilder.WriteString(fmt.Sprintf("\nFile: %s, Line: %d\n", filePath, line))
				diffBuilder.WriteString("  Expected:\n")
				for _, msg := range expectedMessages {
					diffBuilder.WriteString(fmt.Sprintf("    - %s\n", msg))
				}
				diffBuilder.WriteString("  Got:\n")
				for _, msg := range raisedMessages {
					diffBuilder.WriteString(fmt.Sprintf("    - %s\n", msg))
				}
			}
		}

		// Check for unexpected issues
		for line, raisedMessages := range raisedFileIssues {
			if _, exists := expectedFileIssues[line]; !exists {
				diffBuilder.WriteString(fmt.Sprintf("\nFile: %s, Line: %d\n", filePath, line))
				diffBuilder.WriteString("  Expected: no issues\n")
				diffBuilder.WriteString("  Got:\n")
				for _, msg := range raisedMessages {
					diffBuilder.WriteString(fmt.Sprintf("    - %s\n", msg))
				}
			}
		}
	}

	// Check for issues in unexpected files
	for filePath, raisedFileIssues := range *raisedIssues {
		if _, exists := (*expectedIssues)[filePath]; !exists {
			diffBuilder.WriteString(fmt.Sprintf("\nUnexpected file with issues: %s\n", filePath))
			for line, messages := range raisedFileIssues {
				diffBuilder.WriteString(fmt.Sprintf("  Line %d:\n", line))
				for _, msg := range messages {
					diffBuilder.WriteString(fmt.Sprintf("    - %s\n", msg))
				}
			}
		}
	}

	return diffBuilder.String()
}

// Helper function to compare two slices of messages
func messagesEqual(expected, actual []string) bool {
	if len(expected) != len(actual) {
		return false
	}
	sort.Strings(expected)
	sort.Strings(actual)
	return slicesEqual(expected, actual)
}

// Helper function to compare two sorted slices
func slicesEqual(a, b []string) bool {
	for i := range a {
		// if message is empty, don't match
		if a[i] != "" && a[i] != b[i] {
			return false
		}
	}
	return true
}

func getExpectedIssuesInDir(testDir string, fileFilter func(string) bool) (map[string]map[int][]string, error) {
	// map of test file path to map of line number to issue message
	// {"file.test.ext": {1: {"issue1 message"}, {"issue2 message"}}}
	expectedIssues := make(map[string]map[int][]string)
	err := filepath.Walk(testDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		if info.IsDir() {
			return nil
		}

		if !strings.HasSuffix(path, fmt.Sprintf(".test%s", filepath.Ext(path))) {
			return nil
		}

		if fileFilter != nil && !fileFilter(path) {
			return nil
		}

		// load the pragmas (<commentIdentifier> <expect-error>) from the test file
		file, err := ParseFile(path)
		if err != nil {
			// skip the file if it can't be parsed
			return nil
		}

		query, err := sitter.NewQuery([]byte("(comment) @pragma"), file.Language.Grammar())
		if err != nil {
			return nil
		}

		expectedIssues[path] = getExpectedIssuesInFile(file, query)

		return nil
	})
	if err != nil {
		return expectedIssues, err
	}

	return expectedIssues, nil
}

func getExpectedIssuesInFile(file *ParseResult, query *sitter.Query) map[int][]string {
	commentIdentifier := GetEscapedCommentIdentifierFromPath(file.FilePath)

	pattern := fmt.Sprintf(`^%s\s+<expect-error>\s*(?P<message>.*)$`, commentIdentifier)
	pragmaRegexp := regexp.MustCompile(pattern)

	expectedIssues := map[int][]string{}
	cursor := sitter.NewQueryCursor()
	cursor.Exec(query, file.Ast)
	for {
		m, ok := cursor.NextMatch()

		if !ok {
			break
		}

		for _, capture := range m.Captures {
			captureName := query.CaptureNameForId(capture.Index)
			if captureName != "pragma" {
				continue
			}
			expectedLine := -1
			pragma := capture.Node.Content(file.Source)
			prevNode := capture.Node.PrevSibling()
			if prevNode != nil && (prevNode.EndPoint().Row == capture.Node.StartPoint().Row) {
				// if the comment is on the same line as the troublesome code,
				// the line number of the issue is the same as the line number of the comment
				expectedLine = int(prevNode.StartPoint().Row) + 1
			} else {
				// +2 because the pragma is on the line above the expected issue,
				// and the line number is 0-indexed
				expectedLine = int(capture.Node.StartPoint().Row) + 2
			}
			matches := pragmaRegexp.FindAllStringSubmatch(pragma, -1)
			if matches == nil {
				continue
			}

			message := ""
			for _, match := range matches {
				for i, group := range pragmaRegexp.SubexpNames() {
					if i == 0 || group == "" {
						continue
					}

					if group == "message" {
						message = match[i]
					}
				}

				if _, ok := expectedIssues[expectedLine]; !ok {
					expectedIssues[expectedLine] = []string{}
				}

				expectedIssues[expectedLine] = append(expectedIssues[expectedLine], message)
			}
		}
	}
	return expectedIssues
}

func RunAnalyzerTests(testDir string, analyzers []*Analyzer) (string, string, bool, error) {
	log := strings.Builder{}

	// if there's a test file in the testDir for which there's no analyzer,
	// it's most likely a YAML checker test, so skip it

	likelyTestFiles := []string{}
	for _, analyzer := range analyzers {
		likelyTestFiles = append(likelyTestFiles, fmt.Sprintf("%s.test%s", analyzer.Name, GetExtFromLanguage(analyzer.Language)))
	}

	fileFilter := func(path string) bool {
		for _, likelyTestFile := range likelyTestFiles {
			if strings.HasSuffix(path, likelyTestFile) {
				return true
			}
		}
		return false
	}

	passed := true
	expectedIssues, err := getExpectedIssuesInDir(testDir, fileFilter)
	if err != nil {
		err = fmt.Errorf("error getting expected issues in dir %s: %v", testDir, err)
		return "", "", false, err
	}

	raisedIssues, err := RunAnalyzers(testDir, analyzers, fileFilter)
	if err != nil {
		err = fmt.Errorf("error running tests on dir %s: %v", testDir, err)
		return "", "", false, err
	}

	analyzerIssueMap := make(map[string]int)
	for _, analyzer := range analyzers {
		analyzerIssueMap[analyzer.Name] = 0
	}

	raisedIssuesMap := make(map[string]map[int][]string)
	for _, issue := range raisedIssues {
		analyzerIssueMap[*issue.Id]++

		if _, ok := raisedIssuesMap[issue.Filepath]; !ok {
			raisedIssuesMap[issue.Filepath] = make(map[int][]string)
		}

		line := int(issue.Node.Range().StartPoint.Row + 1)
		if _, ok := raisedIssuesMap[issue.Filepath][line]; !ok {
			raisedIssuesMap[issue.Filepath][line] = []string{}
		}

		raisedIssuesMap[issue.Filepath][line] = append(raisedIssuesMap[issue.Filepath][line], fmt.Sprintf("%s: %s", *issue.Id, issue.Message))
	}

	for analyzerId, issueCount := range analyzerIssueMap {
		if issueCount == 0 {
			log.Write([]byte(fmt.Sprintf("  No tests found for analyzer %s\n", analyzerId)))
			passed = false
		} else {
			log.Write([]byte(fmt.Sprintf("  Running tests for analyzer %s\n", analyzerId)))
		}
	}

	// verify issues raised are as expected from the test files
	diff := verifyIssues(&expectedIssues, &raisedIssuesMap)
	if diff != "" {
		passed = false
	}

	return diff, log.String(), passed, nil
}

type YamlTestCase struct {
	YamlCheckerPath string
	TestFile        string
}

func RunYamlTests(testDir string) (passed bool, err error) {
	tests, err := FindYamlTestFiles(testDir)
	if err != nil {
		return false, err
	}

	if len(tests) == 0 {
		return false, fmt.Errorf("no test files found")
	}

	passed = true
	for _, test := range tests {
		if test.TestFile == "" {
			fmt.Fprintf(os.Stderr, "No test file found for checker '%s'\n", test.YamlCheckerPath)
			continue
		}

		fmt.Fprintf(os.Stderr, "Running test case: %s\n", filepath.Base(test.YamlCheckerPath))

		checker, _, err := ReadFromFile(test.YamlCheckerPath)
		if err != nil {
			return false, err
		}

		want, err := findExpectedLines(test.TestFile)
		if err != nil {
			return false, err
		}

		gotIssues, err := RunAnalyzers(test.TestFile, []*Analyzer{&checker}, nil)
		if err != nil {
			return false, err
		}

		var got []int
		for _, issue := range gotIssues {
			got = append(got, int(issue.Node.Range().StartPoint.Row)+1)
		}

		slices.Sort(got)

		if len(want) != len(got) {
			testName := filepath.Base(test.YamlCheckerPath)
			message := fmt.Sprintf(
				"(%s): expected issues on the following lines: %v\nbut issues were raised on lines: %v\n",
				testName,
				want,
				got,
			)
			fmt.Fprintf(os.Stderr, "%s", message)
			passed = false
			continue
		}
		for j := 0; j < len(want); j++ {
			if want[j] != got[j] {
				testName := filepath.Base(test.YamlCheckerPath)
				message := fmt.Sprintf(
					"(%s): expected issue on line %d, but next occurrence is on line %d\n",
					testName,
					want[j],
					got[j],
				)
				fmt.Fprintf(os.Stderr, "%s\n", message)
				passed = false
			}

		}
	}

	return passed, nil
}

func FindYamlTestFiles(testDir string) ([]YamlTestCase, error) {
	var pairs []YamlTestCase

	err := filepath.Walk(testDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return nil
		}

		if info.IsDir() {
			return nil
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}

		fileExt := filepath.Ext(path)
		isYamlFile := fileExt == ".yaml" || fileExt == ".yml"
		if !isYamlFile {
			return nil
		}

		patternChecker, _, err := ReadFromFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid checker '%s': %s\n", filepath.Base(path), err.Error())
			return nil
		}

		testFile := strings.TrimSuffix(path, fileExt) + ".test" + GetExtFromLanguage(patternChecker.Language)

		if _, err := os.Stat(testFile); os.IsNotExist(err) {
			testFile = ""
		}

		pairs = append(pairs, YamlTestCase{YamlCheckerPath: path, TestFile: testFile})
		return nil
	})

	return pairs, err
}

func findExpectedLines(filePath string) ([]int, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var expectedLines []int
	scanner := bufio.NewScanner(file)

	lineNumber := 0
	for scanner.Scan() {
		text := strings.ToLower(scanner.Text())
		lineNumber++
		if strings.Contains(text, "<expect-error>") || strings.Contains(text, "<expect error>") {
			expectedLines = append(expectedLines, lineNumber+1)
		}
	}

	// Check for scanner errors
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return expectedLines, nil
}
