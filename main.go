package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/ktr0731/go-fuzzyfinder"
	"github.com/mitchellh/go-homedir"
	gitlab "github.com/xanzy/go-gitlab"
)

func findRepo(path string) (*git.Repository, error) {
	repo, err := git.PlainOpen(path)
	if err != nil && path != "/" {
		repo, err = findRepo(filepath.Dir(path))
	}
	if err != nil {
		return nil, fmt.Errorf("no git repository in %q: %w", path, err)
	}
	return repo, nil
}

type gitlabClient struct {
	gitlab *gitlab.Client
}

func (c gitlabClient) getProjectFromOrigin(originURL *url.URL) (*gitlab.Project, error) {
	projectPath := strings.TrimSuffix(originURL.Path, ".git")
	projectName := filepath.Base(projectPath)
	projects, _, err := c.gitlab.Projects.ListProjects(
		&gitlab.ListProjectsOptions{Search: gitlab.String(projectName)},
	)
	if err != nil {
		return &gitlab.Project{}, fmt.Errorf("failed to list projects: %w", err)
	}
	for _, project := range projects {
		if "/"+project.PathWithNamespace == projectPath {
			return project, nil
		}
	}
	return nil, fmt.Errorf("could not find project")
}

type issueLabel struct {
	ID          int
	Name        string
	Description string
}

var noLabels = []issueLabel{{ID: 0, Name: "non-existant"}}

func (c gitlabClient) getIssueLabels(project *gitlab.Project) ([]issueLabel, error) {
	l := []issueLabel{}
	labels, _, err := c.gitlab.Labels.ListLabels(project.ID, &gitlab.ListLabelsOptions{})
	if err != nil {
		return l, err
	}
	for _, label := range labels {
		l = append(l, issueLabel{ID: label.ID, Name: label.Name, Description: label.Description})
	}
	return l, nil
}

type issueMilestone struct {
	ID   int
	Name string
}

var noMilestone = issueMilestone{ID: 0, Name: "non-existant"}

func (c gitlabClient) getIssueMilestones(project *gitlab.Project) ([]issueMilestone, error) {
	m := []issueMilestone{}
	milestones, _, err := c.gitlab.Milestones.ListMilestones(project.ID, &gitlab.ListMilestonesOptions{State: gitlab.String("active")})
	if err != nil {
		return m, err
	}
	for _, milestone := range milestones {
		m = append(m, issueMilestone{ID: milestone.ID, Name: milestone.Title})
	}
	return m, nil
}

type issueTemplate struct {
	Name    string
	Content []byte
}

func getLocalIssueTemplates() ([]issueTemplate, error) {
	issueTemplates := []issueTemplate{}
	home, err := homedir.Dir()
	if err != nil {
		return issueTemplates, fmt.Errorf("could not get home-dir: %w", err)
	}
	localTemplateDir := filepath.Join(home, ".config", "gitlab", "issue_templates")
	err = os.MkdirAll(localTemplateDir, os.ModePerm)
	if err != nil {
		return issueTemplates, fmt.Errorf("couuld not make dir %q: %w", localTemplateDir, err)
	}
	files, err := ioutil.ReadDir(localTemplateDir)
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".md") {
			continue
		}
		b, err := ioutil.ReadFile(filepath.Join(localTemplateDir, file.Name()))
		if err != nil {
			return issueTemplates, fmt.Errorf("could not read file %s: %w", file.Name(), err)
		}
		issueTemplates = append(issueTemplates, issueTemplate{
			Name:    strings.TrimSuffix(file.Name(), ".md") + " [local]",
			Content: b,
		})
	}
	return issueTemplates, nil
}

func (c gitlabClient) getIssueTemplates(project *gitlab.Project) ([]issueTemplate, error) {
	issueTemplates := []issueTemplate{
		{
			Name:    "BLANK",
			Content: []byte{},
		},
	}
	localIssueTemplates, err := getLocalIssueTemplates()
	if err != nil {
		return issueTemplates, fmt.Errorf("could not get local issue templates: %w", err)
	}
	issueTemplates = append(issueTemplates, localIssueTemplates...)
	nodes, _, err := c.gitlab.Repositories.ListTree(
		project.ID,
		&gitlab.ListTreeOptions{
			Ref:  gitlab.String(project.DefaultBranch),
			Path: gitlab.String(".gitlab/issue_templates"),
		},
	)
	if err != nil {
		return issueTemplates, fmt.Errorf("error fetching files from issue_templates: %w", err)
	}
	for _, node := range nodes {
		if !strings.HasSuffix(node.Path, ".md") {
			continue
		}
		file, _, err := c.gitlab.RepositoryFiles.GetFile(
			project.ID,
			node.Path,
			&gitlab.GetFileOptions{Ref: gitlab.String(project.DefaultBranch)},
		)
		if err != nil {
			return issueTemplates, fmt.Errorf("error fetching file %s from issue_templates: %w", node.Path, err)
		}
		content, err := base64.StdEncoding.DecodeString(file.Content)
		if err != nil {
			return issueTemplates, fmt.Errorf("error decoding file %s from issue_templates: %w", node.Path, err)
		}
		issueTemplates = append(issueTemplates, issueTemplate{Name: strings.TrimSuffix(file.FileName, ".md"), Content: content})
	}
	return issueTemplates, nil
}

func getEditor(repository *git.Repository) (string, error) {
	gitEditor := os.Getenv("GIT_EDITOR")
	if gitEditor != "" {
		return gitEditor, nil
	}
	cfg, err := repository.ConfigScoped(config.GlobalScope)
	if err != nil {
		return "", fmt.Errorf("could not get git config: %w", err)
	}
	if cfg.Raw.HasSection("core") {
		if cfg.Raw.Section("core").HasOption("editor") {
			return cfg.Raw.Section("core").Option("editor"), nil
		}
	}
	gitEditor = os.Getenv("VISUAL")
	if gitEditor != "" {
		return gitEditor, nil
	}
	gitEditor = os.Getenv("EDITOR")
	if gitEditor != "" {
		return gitEditor, nil
	}
	return "vi", nil
}

func (c gitlabClient) createIssueFromTemplate(repository *git.Repository, project *gitlab.Project, template issueTemplate) (issue *gitlab.Issue, err error) {
	issue = &gitlab.Issue{}
	file, err := ioutil.TempFile("", fmt.Sprintf("*_%s_%s_pre-submit.md", project.Name, template.Name))
	if err != nil {
		return issue, fmt.Errorf("could not create temporary issue-description file: %w", err)
	}
	buf := bytes.Buffer{}
	buf.WriteByte('\n')
	buf.WriteByte('\n')
	buf.Write(template.Content)
	_, err = file.Write(buf.Bytes())
	if err != nil {
		return issue, fmt.Errorf("could not prepopulate template: %w", err)
	}
	err = file.Sync()
	if err != nil {
		return issue, fmt.Errorf("could not sync file to disk: %w", err)
	}
	editor, err := getEditor(repository)
	if err != nil {
		return issue, fmt.Errorf("could not get editor: %w", err)
	}
	editorCommand := strings.Split(editor, " ")
	editorCommand = append(editorCommand, file.Name())
	cmd := exec.Command(editorCommand[0], editorCommand[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		return issue, fmt.Errorf("error running editor: %w", err)
	}
	issueContent, err := ioutil.ReadFile(file.Name())
	if err != nil {
		return issue, fmt.Errorf("could not read file: %w (%s)", err, file.Name())
	}
	if bytes.Equal(issueContent, buf.Bytes()) {
		return issue, fmt.Errorf("content of issue has not been changed")
	}
	issueSplit := strings.SplitN(string(issueContent), "\n", 2)
	if len(issueSplit) == 0 {
		return issue, fmt.Errorf("empty issue content")
	}
	if len(issueSplit[0]) == 0 {
		return issue, fmt.Errorf("empty issue title (%s)", file.Name())
	}
	if len(issueSplit) == 1 {
		issueSplit = append(issueSplit, "")
	}
	issue, _, err = c.gitlab.Issues.CreateIssue(project.ID, &gitlab.CreateIssueOptions{Title: gitlab.String(issueSplit[0]), Description: gitlab.String(issueSplit[1])})
	if err != nil {
		return issue, fmt.Errorf("could not create gitlab issue: %w (%s)", err, file.Name())
	}
	err = os.Remove(file.Name()) // remove file once sure of success
	return issue, err
}

func (c gitlabClient) setIssueLabelsMilestones(project *gitlab.Project, issue *gitlab.Issue, labels []issueLabel, milestone issueMilestone) error {
	var labelNames []string
	for _, l := range labels {
		if l.ID != 0 {
			labelNames = append(labelNames, l.Name)
		}
	}
	options := &gitlab.UpdateIssueOptions{AddLabels: labelNames}
	if milestone.ID != 0 {
		options.MilestoneID = gitlab.Int(milestone.ID)
	}
	_, _, err := c.gitlab.Issues.UpdateIssue(project.ID, issue.IID, options)
	return err
}

func main() {
	currentFullPath, err := filepath.Abs(".")
	if err != nil {
		log.Fatalf("Could not get full path of current dir: %s", err)
	}
	repo, err := findRepo(currentFullPath)
	if err != nil {
		log.Fatalf("Error finding git repo in working directory: %s. Please specify project", err)
	}

	originRemote, err := repo.Remote("origin")
	if err != nil {
		log.Fatalf("Error getting remote origin: %s", err)
	}
	origin := originRemote.Config().URLs[0]
	log.Printf("Origin URL: %s", origin)

	originURL, err := url.Parse(origin)
	if err != nil {
		log.Fatalf("Error parsing URL for origin %s: %s", origin, err)
	}
	gitlabBaseURL := url.URL{Scheme: "https", Host: originURL.Host, Path: "/api/v4"}
	// TODO add timeout or context to client upstream
	cli, err := gitlab.NewClient(os.Getenv("GITLAB_TOKEN"), gitlab.WithBaseURL(gitlabBaseURL.String()))
	if err != nil {
		log.Fatalf("Failed to create client: %s", err)
	}
	client := gitlabClient{
		gitlab: cli,
	}
	project, err := client.getProjectFromOrigin(originURL)
	if err != nil {
		log.Fatalf("Failed to get project from origin URL: %s", err)
	}
	log.Printf("Found project: %s", project.HTTPURLToRepo)
	templates, err := client.getIssueTemplates(project)
	if err != nil {
		log.Fatalf("Failed to get issue templates for project: %s", err)
	}
	if len(templates) == 0 {
		log.Println("No issue templates present")
	}
	idx, err := fuzzyfinder.Find(
		templates,
		func(i int) string {
			return templates[i].Name
		},
	)
	if err != nil {
		log.Fatalf("Failed to select template: %s", err)
	}
	log.Printf("Selected template: %s", templates[idx].Name)
	labels, err := client.getIssueLabels(project)
	if err != nil {
		log.Printf("Failed to get issue labels for project: %s", err)
	}
	if len(labels) == 0 {
		log.Println("No issue labels present")
	}

	milestones, err := client.getIssueMilestones(project)
	if err != nil {
		log.Printf("Failed to get issue milestones for project: %s", err)
	}
	if len(milestones) == 0 {
		log.Println("No issue milestones present")
	}

	issue, err := client.createIssueFromTemplate(repo, project, templates[idx])
	if err != nil {
		log.Fatalf("could not create issue: %s", err)
	}
	log.Printf("created: %s", issue.WebURL)
	selectedMilestone := noMilestone
	if len(milestones) > 0 {
		milestoneIdx, _ := fuzzyfinder.Find(
			milestones,
			func(i int) string {
				return milestones[i].Name
			},
		)
		selectedMilestone = milestones[milestoneIdx]
	}
	selectedLabels := noLabels
	if len(labels) > 0 {
		labelIdxs, err := fuzzyfinder.FindMulti(
			labels,
			func(i int) string {
				return fmt.Sprintf("%s: %s", labels[i].Name, labels[i].Description)
			},
		)
		selectedLabels = []issueLabel{}
		if err != nil {
			selectedLabels = noLabels
		}
		for _, idx := range labelIdxs {
			selectedLabels = append(selectedLabels, labels[idx])
		}
	}

	err = client.setIssueLabelsMilestones(project, issue, selectedLabels, selectedMilestone)
	if err != nil {
		log.Fatalf("could not add labels/milestones to issue: %s", err)
	}
}
