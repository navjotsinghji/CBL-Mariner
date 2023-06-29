// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/exe"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/logger"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/packagerepo/repocloner/rpmrepocloner"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/packagerepo/repoutils"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkggraph"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/pkgjson"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/rpm"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/internal/timestamp"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/pkg/profile"
	"github.com/microsoft/CBL-Mariner/toolkit/tools/scheduler/schedulerutils"

	"gonum.org/v1/gonum/graph"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	app = kingpin.New("graphpkgfetcher", "A tool to download a unresolved packages in a graph into a given directory.")

	inputGraph  = exe.InputStringFlag(app, "Path to the graph file to read")
	outputGraph = exe.OutputFlag(app, "Updated graph file with unresolved nodes marked as resolved")
	outDir      = exe.OutputDirFlag(app, "Directory to download packages into.")

	existingRpmDir          = app.Flag("rpm-dir", "Directory that contains already built RPMs. Should contain top level directories for architecture.").Required().ExistingDir()
	existingToolchainRpmDir = app.Flag("toolchain-rpms-dir", "Directory that contains already built toolchain RPMs. Should contain top level directories for architecture.").Required().ExistingDir()
	tmpDir                  = app.Flag("tmp-dir", "Directory to store temporary files while downloading.").String()

	workertar            = app.Flag("tdnf-worker", "Full path to worker_chroot.tar.gz").Required().ExistingFile()
	repoFiles            = app.Flag("repo-file", "Full path to a repo file").Required().ExistingFiles()
	usePreviewRepo       = app.Flag("use-preview-repo", "Pull packages from the upstream preview repo").Bool()
	disableDefaultRepos  = app.Flag("disable-default-repos", "Disable pulling packages from PMC repos").Bool()
	disableUpstreamRepos = app.Flag("disable-upstream-repos", "Disables pulling packages from upstream repos").Bool()
	toolchainManifest    = app.Flag("toolchain-manifest", "Path to a list of RPMs which are created by the toolchain. Will mark RPMs from this list as prebuilt.").ExistingFile()

	tlsClientCert = app.Flag("tls-cert", "TLS client certificate to use when downloading files.").String()
	tlsClientKey  = app.Flag("tls-key", "TLS client key to use when downloading files.").String()

	stopOnFailure = app.Flag("stop-on-failure", "Stop if failed to cache all unresolved nodes.").Bool()

	inputSummaryFile  = app.Flag("input-summary-file", "Path to a file with the summary of packages cloned to be restored").String()
	outputSummaryFile = app.Flag("output-summary-file", "Path to save the summary of packages cloned").String()

	logFile       = exe.LogFileFlag(app)
	logLevel      = exe.LogLevelFlag(app)
	profFlags     = exe.SetupProfileFlags(app)
	timestampFile = app.Flag("timestamp-file", "File that stores timestamps for this program.").String()
)

func main() {
	app.Version(exe.ToolkitVersion)
	kingpin.MustParse(app.Parse(os.Args[1:]))
	logger.InitBestEffort(*logFile, *logLevel)

	prof, err := profile.StartProfiling(profFlags)
	if err != nil {
		logger.Log.Warnf("Could not start profiling: %s", err)
	}
	defer prof.StopProfiler()

	timestamp.BeginTiming("graphpkgfetcher", *timestampFile)
	defer timestamp.CompleteTiming()

	err = fetchPackages()
	if err != nil {
		logger.Log.Fatalf("Failed to fetch packages. Error: %s", err)
	}
}

func fetchPackages() (err error) {
	var cloner *rpmrepocloner.RpmRepoCloner
	dependencyGraph, err := pkggraph.ReadDOTGraphFile(*inputGraph)
	if err != nil {
		err = fmt.Errorf("failed to read graph to file:\n%w", err)
		return
	}

	toolchainPackages, err := schedulerutils.ReadReservedFilesList(*toolchainManifest)
	if err != nil {
		err = fmt.Errorf("unable to read toolchain manifest file '%s':\n%w", *toolchainManifest, err)
		return
	}

	/*If there is an existing runNode and then there is one more remote node, donot throw dup error, instead replace the run node with remote node*/
	for _, pkgNode := range dependencyGraph.AllNodes() {
		if pkgNode.Type == pkggraph.TypeRemote {
			dependencyGraph.AddRemoteToLookup(pkgNode, true)
		}
	}

	hasUnresolvedNodes := hasUnresolvedNodes(dependencyGraph)
	if hasUnresolvedNodes {
		// Create the worker environment
		cloner, err = rpmrepocloner.ConstructClonerWithNetwork(*outDir, *tmpDir, *workertar, *existingRpmDir, *existingToolchainRpmDir, *tlsClientCert, *tlsClientKey, *usePreviewRepo, *disableUpstreamRepos, *disableDefaultRepos, *repoFiles)
		if err != nil {
			err = fmt.Errorf("failed to setup new cloner:\n%w", err)
			return err
		}
		defer cloner.Close()
	}

	if hasUnresolvedNodes {
		logger.Log.Info("Found unresolved packages to cache, downloading packages")
		err = resolveGraphNodes(dependencyGraph, *inputSummaryFile, *outputSummaryFile, toolchainPackages, cloner, *stopOnFailure)
		if err != nil {
			err = fmt.Errorf("failed to resolve graph:\n%w", err)
			return err
		}
	} else {
		logger.Log.Info("No unresolved packages to cache")
	}

	// Write the graph to file (even if we are going to download delta RPMs, we want to save the graph with the resolved nodes first)
	err = pkggraph.WriteDOTGraphFile(dependencyGraph, *outputGraph)
	if err != nil {
		err = fmt.Errorf("failed to write cache graph to file:\n%w", err)
		return
	}

	// If we grabbed any RPMs, we need to convert them into a local repo
	if hasUnresolvedNodes {
		err = cloner.ConvertDownloadedPackagesIntoRepo()
		if err != nil {
			err = fmt.Errorf("failed to convert downloaded RPMs into a repo:\n%w", err)
			return err
		}
	}

	return
}

// hasUnresolvedNodes scans through the graph to see if there is anything to do
func hasUnresolvedNodes(graph *pkggraph.PkgGraph) bool {
	for _, n := range graph.AllRunNodes() {
		if n.State == pkggraph.StateUnresolved {
			return true
		}
	}
	return false
}

func findUnresolvedNodes(runNodes []*pkggraph.PkgNode) (unreslovedNodes []*pkggraph.PkgNode) {
	for _, n := range runNodes {
		if n.State == pkggraph.StateUnresolved {
			unreslovedNodes = append(unreslovedNodes, n)
		}
	}
	return
}

// resolveGraphNodes scans a graph and for each unresolved node in the graph clones the RPMs needed
// to satisfy it.
func resolveGraphNodes(dependencyGraph *pkggraph.PkgGraph, inputSummaryFile, outputSummaryFile string, toolchainPackages []string, cloner *rpmrepocloner.RpmRepoCloner, stopOnFailure bool) (err error) {
	const downloadDependencies = true
	timestamp.StartEvent("Clone packages", nil)
	defer timestamp.StopEvent(nil)
	cachingSucceeded := true
	if strings.TrimSpace(inputSummaryFile) == "" {
		// Cache an RPM for each unresolved node in the graph.
		fetchedPackages := make(map[string]bool)
		prebuiltPackages := make(map[string]bool)
		unresolvedNodes := findUnresolvedNodes(dependencyGraph.AllRunNodes())

		timestamp.StartEvent("clone graph", nil)

		for _, n := range unresolvedNodes {
			resolveErr := resolveSingleNode(cloner, n, downloadDependencies, toolchainPackages, fetchedPackages, prebuiltPackages, *outDir)
			// Failing to clone a dependency should not halt a build.
			// The build should continue and attempt best effort to build as many packages as possible.
			if resolveErr != nil {
				logger.Log.Warnf("Failed to resolve graph node '%s':\n%s", n, resolveErr)
				cachingSucceeded = false
				errorMessage := strings.Builder{}
				errorMessage.WriteString(fmt.Sprintf("Failed to resolve all nodes in the graph while resolving '%s'\n", n))
				errorMessage.WriteString("Nodes which have this as a dependency:\n")
				for _, dependant := range graph.NodesOf(dependencyGraph.To(n.ID())) {
					errorMessage.WriteString(fmt.Sprintf("\t'%s' depends on '%s'\n", dependant.(*pkggraph.PkgNode), n))
				}
				logger.Log.Debugf(errorMessage.String())
			}
		}
		timestamp.StopEvent(nil) // clone graph
	} else {
		timestamp.StartEvent("restore packages", nil)

		// If an input summary file was provided, simply restore the cache using the file.
		err = repoutils.RestoreClonedRepoContents(cloner, inputSummaryFile)
		cachingSucceeded = err == nil

		timestamp.StopEvent(nil) // restore packages
	}
	if stopOnFailure && !cachingSucceeded {
		return fmt.Errorf("failed to cache unresolved nodes")
	}

	if strings.TrimSpace(outputSummaryFile) != "" {
		err = repoutils.SaveClonedRepoContents(cloner, outputSummaryFile)
		if err != nil {
			logger.Log.Errorf("Failed to save cloned repo contents.")
			return
		}
	}

	return
}

// resolveSingleNode caches the RPM for a single node.
// It will modify fetchedPackages on a successful package clone.
func resolveSingleNode(cloner *rpmrepocloner.RpmRepoCloner, node *pkggraph.PkgNode, cloneDeps bool, toolchainPackages []string, fetchedPackages, prebuiltPackages map[string]bool, outDir string) (err error) {
	logger.Log.Debugf("Adding node %s to the cache", node.FriendlyName())

	logger.Log.Debugf("Searching for a package which supplies: %s", node.VersionedPkg.Name)
	// Resolve nodes to exact package names so they can be referenced in the graph.
	resolvedPackages, err := cloner.WhatProvides(node.VersionedPkg)
	if err != nil {
		msg := fmt.Sprintf("Failed to resolve (%s) to a package. Error: %s", node.VersionedPkg, err)
		// It is not an error if an implicit node could not be resolved as it may become available later in the build.
		// If it does not become available scheduler will print an error at the end of the build.
		if node.Implicit {
			logger.Log.Debug(msg)
		} else {
			logger.Log.Error(msg)
		}
		return
	}

	if len(resolvedPackages) == 0 {
		return fmt.Errorf("failed to find any packages providing '%v'", node.VersionedPkg)
	}

	preBuilt := false
	for _, resolvedPackage := range resolvedPackages {
		if !fetchedPackages[resolvedPackage] {
			desiredPackage := &pkgjson.PackageVer{
				Name: resolvedPackage,
			}

			preBuilt, err = cloner.Clone(cloneDeps, desiredPackage)
			if err != nil {
				err = fmt.Errorf("failed to clone '%s' from RPM repo:\n%w", resolvedPackage, err)
				return
			}
			fetchedPackages[resolvedPackage] = true
			prebuiltPackages[resolvedPackage] = preBuilt

			logger.Log.Debugf("Fetched '%s' as potential candidate (is pre-built: %v).", resolvedPackage, prebuiltPackages[resolvedPackage])
		}
	}

	err = assignRPMPath(node, outDir, resolvedPackages)
	if err != nil {
		err = fmt.Errorf("failed to find an RPM to provide '%s':\n%w", node.VersionedPkg.Name, err)
		return
	}

	// If a package is  available locally, and it is part of the toolchain, mark it as a prebuilt so the scheduler knows it can use it
	// immediately (especially for dynamic generator created capabilities)
	if (preBuilt || prebuiltPackages[node.RpmPath]) && isToolchainPackage(node.RpmPath, toolchainPackages) {
		logger.Log.Debugf("Using a prebuilt toolchain package to resolve this dependency")
		prebuiltPackages[node.RpmPath] = true
		node.State = pkggraph.StateUpToDate
		node.Type = pkggraph.TypePreBuilt
	} else {
		node.State = pkggraph.StateCached
	}

	logger.Log.Infof("Choosing '%s' to provide '%s'.", filepath.Base(node.RpmPath), node.VersionedPkg.Name)

	return
}

func assignRPMPath(node *pkggraph.PkgNode, outDir string, resolvedPackages []string) (err error) {
	rpmPaths := []string{}
	for _, resolvedPackage := range resolvedPackages {
		rpmPaths = append(rpmPaths, rpmPackageToRPMPath(resolvedPackage, outDir))
	}

	node.RpmPath = rpmPaths[0]
	if len(rpmPaths) > 1 {
		var resolvedRPMs []string
		logger.Log.Debugf("Found %d candidates. Resolving.", len(rpmPaths))

		resolvedRPMs, err = rpm.ResolveCompetingPackages(*tmpDir, rpmPaths...)
		if err != nil {
			logger.Log.Errorf("Failed while trying to pick an RPM providing '%s' from the following RPMs: %v", node.VersionedPkg.Name, rpmPaths)
			return
		}

		resolvedRPMsCount := len(resolvedRPMs)
		if resolvedRPMsCount == 0 {
			logger.Log.Errorf("Failed while trying to pick an RPM providing '%s'. No RPM can be installed from the following: %v", node.VersionedPkg.Name, rpmPaths)
			return
		}

		if resolvedRPMsCount > 1 {
			logger.Log.Warnf("Found %d candidates to provide '%s'. Picking the first one.", resolvedRPMsCount, node.VersionedPkg.Name)
		}

		node.RpmPath = rpmPackageToRPMPath(resolvedRPMs[0], outDir)
	}

	return
}

func rpmPackageToRPMPath(rpmPackage, outDir string) string {
	// Construct the rpm path of the cloned package.
	rpmName := fmt.Sprintf("%s.rpm", rpmPackage)
	return filepath.Join(outDir, rpmName)
}

func isToolchainPackage(rpmPath string, toolchainRPMs []string) bool {
	base := filepath.Base(rpmPath)
	for _, t := range toolchainRPMs {
		if t == base {
			return true
		}
	}
	return false
}
