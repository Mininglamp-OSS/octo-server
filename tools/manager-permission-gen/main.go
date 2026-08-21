package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Mininglamp-OSS/octo-server/pkg/authz"
)

func main() {
	manifestPath := flag.String("manifest", "authz/manager-permissions.yaml", "permission manifest path")
	goOutputPath := flag.String("go-output", "pkg/authz/permissions_generated.go", "generated Go output path")
	jsonOutputPath := flag.String("json-output", "authz/generated/manager-permissions.json", "generated JSON output path")
	repositoryRoot := flag.String("repo", ".", "repository root")
	check := flag.Bool("check", false, "validate source inventory and generated file consistency")
	flag.Parse()

	if *check {
		if err := checkContract(*repositoryRoot, *manifestPath, *goOutputPath, *jsonOutputPath); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := authz.WriteGeneratedFiles(*manifestPath, *goOutputPath, *jsonOutputPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkContract(repositoryRoot, manifestPath, goOutputPath, jsonOutputPath string) error {
	manifest, err := authz.LoadManifest(manifestPath)
	if err != nil {
		return err
	}
	if err := authz.ValidateManifest(manifest); err != nil {
		return err
	}
	if err := authz.ValidateCriticalPermissions(manifest); err != nil {
		return err
	}
	if err := authz.ValidateRecognizedGateLocations(repositoryRoot); err != nil {
		return err
	}
	gates, err := authz.ScanDirectGates(repositoryRoot)
	if err != nil {
		return err
	}
	routes, err := authz.ScanManagerRoutes(repositoryRoot, gates)
	if err != nil {
		return err
	}
	platformGates, err := authz.PlatformGates(gates, routes)
	if err != nil {
		return err
	}
	if err := authz.ValidateGateInventory(platformGates, manifest.GateSites); err != nil {
		return err
	}
	exclusions := append(authz.ManagerRouteBoundaryExclusions(), authz.ManagerRBACMetaSurfaceExclusions()...)
	if err := authz.ValidateRouteCoverage(routes, manifest.Operations, exclusions); err != nil {
		return err
	}
	return authz.CheckGeneratedFiles(manifestPath, goOutputPath, jsonOutputPath)
}
