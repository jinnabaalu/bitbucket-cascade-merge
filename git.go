package main

import (
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	git "github.com/libgit2/git2go/v34"
)

const (
	DefaultMaster                = "master"
	DefaultRemoteName            = "origin"
	DefaultRemoteReferencePrefix = "refs/heads/"
	DefaultCommitReferenceName   = "HEAD"
)

type Client struct {
	Repository      *git.Repository
	RemoteCallbacks git.RemoteCallbacks
	Author          *Author
}

type Credentials struct {
	Username string
	Password string
}

type ClientOptions struct {
	Path        string
	URL         string
	Author      *Author
	Credentials *Credentials
}

func (c *Client) CascadeMerge(branchName string, options *CascadeOptions) *CascadeMergeState {

	if options == nil {
		options = &CascadeOptions{
			DevelopmentName: "develop",
			ReleasePrefix:   "release/",
		}
	}

	err := c.RemoveLocalBranches()
	if err != nil {
		return &CascadeMergeState{error: err}
	}

	err = c.Fetch()
	if err != nil {
		return &CascadeMergeState{error: err}
	}

	cascade, err := c.BuildCascade(options, branchName)
	log.Printf("Cascade List: %+v", cascade)
	if err != nil {
		return &CascadeMergeState{error: err}
	}

	source := branchName

	err = c.Checkout(source)
	if err != nil {
		return &CascadeMergeState{error: err}
	}

	err = c.Reset(source)
	if err != nil {
		return &CascadeMergeState{error: err}
	}

	for target := cascade.Next(); target != ""; target = cascade.Next() {
		err = c.Checkout(target)
		if err != nil {
			return &CascadeMergeState{Source: source, Target: target, error: err}
		}

		err = c.Reset(target)
		if err != nil {
			return &CascadeMergeState{Source: source, Target: target, error: err}
		}

		err = c.MergeBranches(source, target)
		if err != nil {
			return &CascadeMergeState{Source: source, Target: target, error: err}
		}

		err := c.Push(target)
		if err != nil {
			return &CascadeMergeState{Source: source, Target: target, error: err}
		}

		source = target
	}

	return nil
}

func (c *Client) Commit(message string, path ...string) (*git.Oid, error) {
	index, err := c.Repository.Index()
	if err != nil {
		return nil, err
	}
	defer index.Free()

	var parent *git.Commit
	head, _ := c.Repository.Head()
	if head != nil {
		parent, err = c.Repository.LookupCommit(head.Target())
		if err != nil {
			return nil, err
		}
		defer parent.Free()
		defer head.Free()
	}

	for _, p := range path {
		err = index.AddByPath(p)
		if err != nil {
			return nil, err
		}
	}

	oid, err := index.WriteTree()
	if err != nil {
		return nil, err
	}

	err = index.Write()
	if err != nil {
		return nil, err
	}

	tree, err := c.Repository.LookupTree(oid)
	if err != nil {
		return nil, err
	}
	defer tree.Free()

	signature := &git.Signature{
		Name:  c.Author.Name,
		Email: c.Author.Email,
		When:  time.Now(),
	}

	if parent != nil {
		return c.Repository.CreateCommit(DefaultCommitReferenceName, signature, signature, message, tree, parent)
	} else {
		return c.Repository.CreateCommit(DefaultCommitReferenceName, signature, signature, message, tree)
	}
}

func (c *Client) Checkout(branchName string) error {
	checkoutOpts := &git.CheckoutOpts{
		Strategy: git.CheckoutSafe | git.CheckoutRecreateMissing | git.CheckoutAllowConflicts | git.CheckoutUseTheirs,
	}

	var commit *git.Commit
	remoteBranch, err := c.Repository.LookupBranch(DefaultRemoteName+"/"+branchName, git.BranchRemote)
	if remoteBranch != nil {
		// read remote branch commit
		commit, err = c.Repository.LookupCommit(remoteBranch.Target())
		if err != nil {
			return err
		}
		defer commit.Free()
		defer remoteBranch.Free()
	} else {
		// read head commit
		head, _ := c.Repository.Head()
		if head != nil {
			commit, err = c.Repository.LookupCommit(head.Target())
			if err != nil {
				return err
			}
			defer commit.Free()
			defer head.Free()
		}
	}

	localBranch, _ := c.Repository.LookupBranch(branchName, git.BranchLocal)
	if localBranch == nil {
		// creating local branch
		localBranch, err = c.Repository.CreateBranch(branchName, commit, false)
		if err != nil {
			return err
		}

		// setting upstream to origin branch
		if remoteBranch != nil {
			err = localBranch.SetUpstream(DefaultRemoteName + "/" + branchName)
			if err != nil {
				return err
			}
		}
	}
	if localBranch == nil {
		return errors.New("error while locating/creating local branch")
	}
	defer localBranch.Free()

	// getting the tree for the branch
	localCommit, err := c.Repository.LookupCommit(localBranch.Target())
	if err != nil {
		return err
	}
	defer localCommit.Free()

	tree, err := c.Repository.LookupTree(localCommit.TreeId())
	if err != nil {
		return err
	}
	defer tree.Free()

	// checkout the tree
	err = c.Repository.CheckoutTree(tree, checkoutOpts)
	if err != nil {
		return err
	}
	// setting the Head to point to our branch
	c.Repository.SetHead("refs/heads/" + branchName)
	return nil
}

func (c *Client) Push(branchName string) error {
	remote, err := c.Repository.Remotes.Lookup(DefaultRemoteName)

	if err != nil {
		return err
	}
	defer remote.Free()

	err = remote.Push([]string{DefaultRemoteReferencePrefix + branchName}, &git.PushOptions{RemoteCallbacks: c.RemoteCallbacks})

	if err != nil {
		return err
	}

	return nil
}

func (c *Client) Fetch() error {
	remote, err := c.Repository.Remotes.Lookup(DefaultRemoteName)
	if err != nil {
		return err
	}
	defer remote.Free()

	var refs []string
	err = remote.Fetch(refs, &git.FetchOptions{RemoteCallbacks: c.RemoteCallbacks, Prune: git.FetchPruneOn}, "")

	if err != nil {
		return err
	}

	return nil
}

// Reset current HEAD to the remote branch
func (c *Client) Reset(branchName string) error {
	branch, err := c.Repository.LookupBranch(fmt.Sprintf("%s/%s", DefaultRemoteName, branchName), git.BranchRemote)
	if err != nil {
		return err
	}
	defer branch.Free()

	commit, err := c.Repository.LookupCommit(branch.Target())
	if err != nil {
		return err
	}
	defer commit.Free()

	err = c.Repository.ResetToCommit(commit, git.ResetHard, &git.CheckoutOpts{})
	if err != nil {
		return err
	}

	return nil
}

// compareVersions compares two version strings (e.g., "1.2.3" and "1.10.0").
// It returns -1 if v1 < v2, 1 if v1 > v2, and 0 if they are equal.
func compareVersions(v1, v2 string) int {
	v1Parts := strings.Split(v1, ".")
	v2Parts := strings.Split(v2, ".")
	for i := 0; i < len(v1Parts) && i < len(v2Parts); i++ {
		n1, _ := strconv.Atoi(v1Parts[i])
		n2, _ := strconv.Atoi(v2Parts[i])
		if n1 < n2 {
			return -1
		} else if n1 > n2 {
			return 1
		}
	}
	if len(v1Parts) < len(v2Parts) {
		return -1
	} else if len(v1Parts) > len(v2Parts) {
		return 1
	}
	return 0
}

func (c *Client) BuildCascade(options *CascadeOptions, startBranch string) (*Cascade, error) {
	cascade := Cascade{
		Branches: make([]string, 0),
		Current:  0,
	}

	iterator, err := c.Repository.NewBranchIterator(git.BranchRemote)
	if err != nil {
		return nil, err
	}

	// Collect release branches and others
	var releaseBranches []string
	iterator.ForEach(func(branch *git.Branch, branchType git.BranchType) error {
		shorthand := branch.Shorthand()
		branchName := strings.TrimPrefix(shorthand, DefaultRemoteName+"/")
		log.Printf("Cascade Branch Name: %s", branchName)
		if branchName == options.DevelopmentName {
			cascade.Append(branchName)
		} else if strings.HasPrefix(branchName, options.ReleasePrefix) {
			releaseBranches = append(releaseBranches, branchName)
		}
		return nil
	})

	// Sort release branches by version
	sort.Slice(releaseBranches, func(i, j int) bool {
		v1 := strings.TrimPrefix(releaseBranches[i], options.ReleasePrefix)
		v2 := strings.TrimPrefix(releaseBranches[j], options.ReleasePrefix)
		return compareVersions(v1, v2) < 0
	})

	// Add sorted release branches to the cascade
	cascade.Branches = append(cascade.Branches, releaseBranches...)

	log.Printf("Cascade List Before Slice: %+v", cascade)
	log.Printf("Start Branch %s", startBranch)
	cascade.Slice(startBranch)

	// Check if DefaultMaster exists in the cascade list
	masterIndex := -1
	for i, branch := range cascade.Branches {
		if branch == DefaultMaster {
			masterIndex = i
			break
		}
	}

	// Move DefaultMaster to the end if it exists
	if masterIndex != -1 {
		cascade.Branches = append(append(cascade.Branches[:masterIndex], cascade.Branches[masterIndex+1:]...), DefaultMaster)
	} else {
		// Add DefaultMaster if not already in the list
		cascade.Append(DefaultMaster)
	}

	log.Printf("Cascade List After Slice : %+v", cascade)
	return &cascade, nil
}

func (c *Client) MergeBranches(sourceBranchName string, destinationBranchName string) error {
	// assuming that these two branches are local already
	log.Printf("Merging from Source: %s branch to Target: %s", sourceBranchName, destinationBranchName)
	sourceBranch, err := c.Repository.LookupBranch(sourceBranchName, git.BranchLocal)
	if err != nil {
		return err
	}
	defer sourceBranch.Free()

	destinationBranch, err := c.Repository.LookupBranch(destinationBranchName, git.BranchLocal)
	if err != nil {
		return err
	}
	defer destinationBranch.Free()

	// assuming we are already checkout as the destination branch
	sourceAnnCommit, err := c.Repository.AnnotatedCommitFromRef(sourceBranch.Reference)
	if err != nil {
		return err
	}
	defer sourceAnnCommit.Free()

	// getting repo head
	head, err := c.Repository.Head()
	if err != nil {
		return err
	}

	// do merge analysis
	mergeHeads := make([]*git.AnnotatedCommit, 1)
	mergeHeads[0] = sourceAnnCommit
	analysis, _, err := c.Repository.MergeAnalysis(mergeHeads)

	// branches are already merged?
	if analysis&git.MergeAnalysisNone != 0 || analysis&git.MergeAnalysisUpToDate != 0 {
		return nil
	}

	// should merge
	if analysis&git.MergeAnalysisNormal == 0 {
		return errors.New("merge analysis returned as not normal merge")
	}

	// options for merge
	mergeOpts, _ := git.DefaultMergeOptions()
	mergeOpts.FileFavor = git.MergeFileFavorNormal
	mergeOpts.TreeFlags = git.MergeTreeFailOnConflict

	// options for checkout
	checkoutOpts := &git.CheckoutOpts{
		Strategy: git.CheckoutSafe | git.CheckoutRecreateMissing | git.CheckoutUseTheirs,
	}

	// merge action
	if err = c.Repository.Merge(mergeHeads, &mergeOpts, checkoutOpts); err != nil {
		return err
	}

	// getting repo index
	index, err := c.Repository.Index()
	if err != nil {
		return err
	}
	defer index.Free()

	// checking for conflicts
	if index.HasConflicts() {
		return errors.New("merge resulted in conflicts, please solve the conflicts before merging")
	}

	// getting last commit from source
	commit, err := c.Repository.LookupCommit(sourceBranch.Target())
	if err != nil {
		return err
	}
	defer commit.Free()

	// getting signature
	signature := commit.Author()

	// writing tree to index
	treeId, err := index.WriteTree()
	if err != nil {
		return err
	}

	// getting the created tree
	tree, err := c.Repository.LookupTree(treeId)
	if err != nil {
		return err
	}
	defer tree.Free()

	// getting head's commit
	currentDestinationCommit, err := c.Repository.LookupCommit(head.Target())
	if err != nil {
		return err
	}

	// commit
	_, err = c.Repository.CreateCommit(DefaultCommitReferenceName, signature, signature, "Automatic merge "+sourceBranchName+" into "+destinationBranchName,
		tree, currentDestinationCommit, commit)
	if err != nil {
		return err
	}

	err = c.Repository.StateCleanup()
	if err != nil {
		return err
	}
	log.Printf("Merging from Source: %s branch to Target: %s is successful", sourceBranchName, destinationBranchName)
	return nil
}

func (c *Client) RemoveLocalBranches() error {
	iterator, err := c.Repository.NewBranchIterator(git.BranchLocal)
	if err != nil {
		return err
	}

	iterator.ForEach(func(branch *git.Branch, branchType git.BranchType) error {
		if DefaultMaster != branch.Shorthand() {
			err = branch.Delete()
			if err != nil {
				return err
			}
		}
		return nil
	})

	return nil
}

func (c *Client) Close() {
	c.Repository.Free()
}

func NewClient(options *ClientOptions) (*Client, error) {

	if options == nil || !options.Validate() {
		return nil, errors.New("invalid client options")
	}

	var r *git.Repository
	var cb git.RemoteCallbacks
	var err error

	// try to open an existing repository
	r, err = git.OpenRepository(options.Path)

	// create fetch options (credentials callback)
	cb = options.CreateRemoteCallbacks()

	if err != nil {
		// try clone the given url with the given credentials
		r, err = git.Clone(options.URL, options.Path, &git.CloneOptions{FetchOptions: git.FetchOptions{RemoteCallbacks: cb}})
		if err != nil {
			return nil, fmt.Errorf("cannot initialize repository at %s : %s", options.URL, err)
		}
	}

	if r == nil {
		return nil, errors.New("error while initializing repository")
	}

	return &Client{
		Repository:      r,
		RemoteCallbacks: cb,
		Author:          options.Author,
	}, nil

}

func (o *ClientOptions) Validate() bool {
	if len(o.URL) > 0 && len(o.Path) > 0 {
		return true
	}
	return false
}

func (o *ClientOptions) CreateRemoteCallbacks() git.RemoteCallbacks {
	if c := o.Credentials; c != nil {
		return git.RemoteCallbacks{
			CredentialsCallback: makeCredentialsCallback(c.Username, c.Password),
		}
	}
	return git.RemoteCallbacks{}
}

func makeCredentialsCallback(username, password string) git.CredentialsCallback {
	return func(url, u string, ct git.CredType) (*git.Cred, error) {
		cred, err := git.NewCredUserpassPlaintext(username, password)
		return cred, err
	}
}
