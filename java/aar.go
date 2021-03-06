// Copyright 2018 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package java

import (
	"android/soong/android"
	"strings"

	"github.com/google/blueprint"
	"github.com/google/blueprint/proptools"
)

type AndroidLibraryDependency interface {
	Dependency
	ExportPackage() android.Path
	ExportedProguardFlagFiles() android.Paths
	ExportedStaticPackages() android.Paths
	ExportedManifest() android.Path
}

func init() {
	android.RegisterModuleType("android_library_import", AARImportFactory)
	android.RegisterModuleType("android_library", AndroidLibraryFactory)
}

//
// AAR (android library)
//

type androidLibraryProperties struct {
	BuildAAR bool `blueprint:"mutated"`
}

type aaptProperties struct {
	// flags passed to aapt when creating the apk
	Aaptflags []string

	// list of directories relative to the Blueprints file containing assets.
	// Defaults to "assets"
	Asset_dirs []string

	// list of directories relative to the Blueprints file containing
	// Android resources
	Resource_dirs []string

	// path to AndroidManifest.xml.  If unset, defaults to "AndroidManifest.xml".
	Manifest *string
}

type aapt struct {
	aaptSrcJar            android.Path
	exportPackage         android.Path
	manifestPath          android.Path
	proguardOptionsFile   android.Path
	rroDirs               android.Paths
	rTxt                  android.Path
	extraAaptPackagesFile android.Path
	isLibrary             bool

	aaptProperties aaptProperties
}

func (a *aapt) ExportPackage() android.Path {
	return a.exportPackage
}

func (a *aapt) aapt2Flags(ctx android.ModuleContext, sdkContext sdkContext, manifestPath android.Path) (flags []string,
	deps android.Paths, resDirs, overlayDirs []globbedResourceDir, rroDirs android.Paths) {

	hasVersionCode := false
	hasVersionName := false
	for _, f := range a.aaptProperties.Aaptflags {
		if strings.HasPrefix(f, "--version-code") {
			hasVersionCode = true
		} else if strings.HasPrefix(f, "--version-name") {
			hasVersionName = true
		}
	}

	var linkFlags []string

	// Flags specified in Android.bp
	linkFlags = append(linkFlags, a.aaptProperties.Aaptflags...)

	linkFlags = append(linkFlags, "--no-static-lib-packages")

	// Find implicit or explicit asset and resource dirs
	assetDirs := android.PathsWithOptionalDefaultForModuleSrc(ctx, a.aaptProperties.Asset_dirs, "assets")
	resourceDirs := android.PathsWithOptionalDefaultForModuleSrc(ctx, a.aaptProperties.Resource_dirs, "res")

	var linkDeps android.Paths

	// Glob directories into lists of paths
	for _, dir := range resourceDirs {
		resDirs = append(resDirs, globbedResourceDir{
			dir:   dir,
			files: androidResourceGlob(ctx, dir),
		})
		resOverlayDirs, resRRODirs := overlayResourceGlob(ctx, dir)
		overlayDirs = append(overlayDirs, resOverlayDirs...)
		rroDirs = append(rroDirs, resRRODirs...)
	}

	var assetFiles android.Paths
	for _, dir := range assetDirs {
		assetFiles = append(assetFiles, androidResourceGlob(ctx, dir)...)
	}

	linkFlags = append(linkFlags, "--manifest "+manifestPath.String())
	linkDeps = append(linkDeps, manifestPath)

	linkFlags = append(linkFlags, android.JoinWithPrefix(assetDirs.Strings(), "-A "))
	linkDeps = append(linkDeps, assetFiles...)

	// SDK version flags
	minSdkVersion := sdkVersionOrDefault(ctx, sdkContext.minSdkVersion())

	linkFlags = append(linkFlags, "--min-sdk-version "+minSdkVersion)
	linkFlags = append(linkFlags, "--target-sdk-version "+minSdkVersion)

	// Version code
	if !hasVersionCode {
		linkFlags = append(linkFlags, "--version-code", ctx.Config().PlatformSdkVersion())
	}

	if !hasVersionName {
		var versionName string
		if ctx.ModuleName() == "framework-res" {
			// Some builds set AppsDefaultVersionName() to include the build number ("O-123456").  aapt2 copies the
			// version name of framework-res into app manifests as compileSdkVersionCodename, which confuses things
			// if it contains the build number.  Use the PlatformVersionName instead.
			versionName = ctx.Config().PlatformVersionName()
		} else {
			versionName = ctx.Config().AppsDefaultVersionName()
		}
		versionName = proptools.NinjaEscape([]string{versionName})[0]
		linkFlags = append(linkFlags, "--version-name ", versionName)
	}

	return linkFlags, linkDeps, resDirs, overlayDirs, rroDirs
}

func (a *aapt) deps(ctx android.BottomUpMutatorContext, sdkContext sdkContext) {
	if !ctx.Config().UnbundledBuild() {
		sdkDep := decodeSdkDep(ctx, sdkContext)
		if sdkDep.frameworkResModule != "" {
			ctx.AddVariationDependencies(nil, frameworkResTag, sdkDep.frameworkResModule)
		}
	}
}

func (a *aapt) buildActions(ctx android.ModuleContext, sdkContext sdkContext, extraLinkFlags ...string) {
	transitiveStaticLibs, staticLibManifests, libDeps, libFlags := aaptLibs(ctx, sdkContext)

	// App manifest file
	manifestFile := proptools.StringDefault(a.aaptProperties.Manifest, "AndroidManifest.xml")
	manifestSrcPath := android.PathForModuleSrc(ctx, manifestFile)

	manifestPath := manifestMerger(ctx, manifestSrcPath, sdkContext, staticLibManifests, a.isLibrary)

	linkFlags, linkDeps, resDirs, overlayDirs, rroDirs := a.aapt2Flags(ctx, sdkContext, manifestPath)

	linkFlags = append(linkFlags, libFlags...)
	linkDeps = append(linkDeps, libDeps...)
	linkFlags = append(linkFlags, extraLinkFlags...)
	if a.isLibrary {
		linkFlags = append(linkFlags, "--static-lib")
	}

	packageRes := android.PathForModuleOut(ctx, "package-res.apk")
	srcJar := android.PathForModuleGen(ctx, "R.jar")
	proguardOptionsFile := android.PathForModuleGen(ctx, "proguard.options")
	rTxt := android.PathForModuleOut(ctx, "R.txt")
	// This file isn't used by Soong, but is generated for exporting
	extraPackages := android.PathForModuleOut(ctx, "extra_packages")

	var compiledResDirs []android.Paths
	for _, dir := range resDirs {
		compiledResDirs = append(compiledResDirs, aapt2Compile(ctx, dir.dir, dir.files).Paths())
	}

	var compiledRes, compiledOverlay android.Paths

	compiledOverlay = append(compiledOverlay, transitiveStaticLibs...)

	if a.isLibrary {
		// For a static library we treat all the resources equally with no overlay.
		for _, compiledResDir := range compiledResDirs {
			compiledRes = append(compiledRes, compiledResDir...)
		}
	} else if len(transitiveStaticLibs) > 0 {
		// If we are using static android libraries, every source file becomes an overlay.
		// This is to emulate old AAPT behavior which simulated library support.
		for _, compiledResDir := range compiledResDirs {
			compiledOverlay = append(compiledOverlay, compiledResDir...)
		}
	} else if len(compiledResDirs) > 0 {
		// Without static libraries, the first directory is our directory, which can then be
		// overlaid by the rest.
		compiledRes = append(compiledRes, compiledResDirs[0]...)
		for _, compiledResDir := range compiledResDirs[1:] {
			compiledOverlay = append(compiledOverlay, compiledResDir...)
		}
	}

	for _, dir := range overlayDirs {
		compiledOverlay = append(compiledOverlay, aapt2Compile(ctx, dir.dir, dir.files).Paths()...)
	}

	aapt2Link(ctx, packageRes, srcJar, proguardOptionsFile, rTxt, extraPackages,
		linkFlags, linkDeps, compiledRes, compiledOverlay)

	a.aaptSrcJar = srcJar
	a.exportPackage = packageRes
	a.manifestPath = manifestPath
	a.proguardOptionsFile = proguardOptionsFile
	a.rroDirs = rroDirs
	a.extraAaptPackagesFile = extraPackages
	a.rTxt = rTxt
}

// aaptLibs collects libraries from dependencies and sdk_version and converts them into paths
func aaptLibs(ctx android.ModuleContext, sdkContext sdkContext) (transitiveStaticLibs, staticLibManifests,
	deps android.Paths, flags []string) {

	var sharedLibs android.Paths

	sdkDep := decodeSdkDep(ctx, sdkContext)
	if sdkDep.useFiles {
		sharedLibs = append(sharedLibs, sdkDep.jars...)
	}

	ctx.VisitDirectDeps(func(module android.Module) {
		var exportPackage android.Path
		aarDep, _ := module.(AndroidLibraryDependency)
		if aarDep != nil {
			exportPackage = aarDep.ExportPackage()
		}

		switch ctx.OtherModuleDependencyTag(module) {
		case libTag, frameworkResTag:
			if exportPackage != nil {
				sharedLibs = append(sharedLibs, exportPackage)
			}
		case staticLibTag:
			if exportPackage != nil {
				transitiveStaticLibs = append(transitiveStaticLibs, exportPackage)
				transitiveStaticLibs = append(transitiveStaticLibs, aarDep.ExportedStaticPackages()...)
				staticLibManifests = append(staticLibManifests, aarDep.ExportedManifest())
			}
		}
	})

	deps = append(deps, sharedLibs...)
	deps = append(deps, transitiveStaticLibs...)

	if len(transitiveStaticLibs) > 0 {
		flags = append(flags, "--auto-add-overlay")
	}

	for _, sharedLib := range sharedLibs {
		flags = append(flags, "-I "+sharedLib.String())
	}

	transitiveStaticLibs = android.FirstUniquePaths(transitiveStaticLibs)

	return transitiveStaticLibs, staticLibManifests, deps, flags
}

type AndroidLibrary struct {
	Library
	aapt

	androidLibraryProperties androidLibraryProperties

	aarFile android.WritablePath

	exportedProguardFlagFiles android.Paths
	exportedStaticPackages    android.Paths
}

func (a *AndroidLibrary) ExportedProguardFlagFiles() android.Paths {
	return a.exportedProguardFlagFiles
}

func (a *AndroidLibrary) ExportedStaticPackages() android.Paths {
	return a.exportedStaticPackages
}

func (a *AndroidLibrary) ExportedManifest() android.Path {
	return a.manifestPath
}

var _ AndroidLibraryDependency = (*AndroidLibrary)(nil)

func (a *AndroidLibrary) DepsMutator(ctx android.BottomUpMutatorContext) {
	a.Module.deps(ctx)
	if !Bool(a.properties.No_framework_libs) && !Bool(a.properties.No_standard_libs) {
		a.aapt.deps(ctx, sdkContext(a))
	}
}

func (a *AndroidLibrary) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	a.isLibrary = true
	a.aapt.buildActions(ctx, sdkContext(a))

	ctx.CheckbuildFile(a.proguardOptionsFile)
	ctx.CheckbuildFile(a.exportPackage)
	ctx.CheckbuildFile(a.aaptSrcJar)

	// apps manifests are handled by aapt, don't let Module see them
	a.properties.Manifest = nil

	a.Module.extraProguardFlagFiles = append(a.Module.extraProguardFlagFiles,
		a.proguardOptionsFile)

	a.Module.compile(ctx, a.aaptSrcJar)

	a.aarFile = android.PathForOutput(ctx, ctx.ModuleName()+".aar")
	var res android.Paths
	if a.androidLibraryProperties.BuildAAR {
		BuildAAR(ctx, a.aarFile, a.outputFile, a.manifestPath, a.rTxt, res)
		ctx.CheckbuildFile(a.aarFile)
	}

	ctx.VisitDirectDeps(func(m android.Module) {
		if lib, ok := m.(AndroidLibraryDependency); ok && ctx.OtherModuleDependencyTag(m) == staticLibTag {
			a.exportedProguardFlagFiles = append(a.exportedProguardFlagFiles, lib.ExportedProguardFlagFiles()...)
			a.exportedStaticPackages = append(a.exportedStaticPackages, lib.ExportPackage())
			a.exportedStaticPackages = append(a.exportedStaticPackages, lib.ExportedStaticPackages()...)
		}
	})

	a.exportedProguardFlagFiles = android.FirstUniquePaths(a.exportedProguardFlagFiles)
	a.exportedStaticPackages = android.FirstUniquePaths(a.exportedStaticPackages)
}

func AndroidLibraryFactory() android.Module {
	module := &AndroidLibrary{}

	module.AddProperties(
		&module.Module.properties,
		&module.Module.deviceProperties,
		&module.Module.protoProperties,
		&module.aaptProperties,
		&module.androidLibraryProperties)

	module.androidLibraryProperties.BuildAAR = true

	android.InitAndroidArchModule(module, android.DeviceSupported, android.MultilibCommon)
	return module
}

//
// AAR (android library) prebuilts
//

type AARImportProperties struct {
	Aars []string

	Sdk_version     *string
	Min_sdk_version *string

	Static_libs []string
	Libs        []string

	// if set to true, run Jetifier against .aar file. Defaults to false.
	Jetifier_enabled *bool
}

type AARImport struct {
	android.ModuleBase
	prebuilt android.Prebuilt

	properties AARImportProperties

	classpathFile         android.WritablePath
	proguardFlags         android.WritablePath
	exportPackage         android.WritablePath
	extraAaptPackagesFile android.WritablePath
	manifest              android.WritablePath

	exportedStaticPackages android.Paths
}

func (a *AARImport) sdkVersion() string {
	return String(a.properties.Sdk_version)
}

func (a *AARImport) minSdkVersion() string {
	if a.properties.Min_sdk_version != nil {
		return *a.properties.Min_sdk_version
	}
	return a.sdkVersion()
}

var _ AndroidLibraryDependency = (*AARImport)(nil)

func (a *AARImport) ExportPackage() android.Path {
	return a.exportPackage
}

func (a *AARImport) ExportedProguardFlagFiles() android.Paths {
	return android.Paths{a.proguardFlags}
}

func (a *AARImport) ExportedStaticPackages() android.Paths {
	return a.exportedStaticPackages
}

func (a *AARImport) ExportedManifest() android.Path {
	return a.manifest
}

func (a *AARImport) Prebuilt() *android.Prebuilt {
	return &a.prebuilt
}

func (a *AARImport) Name() string {
	return a.prebuilt.Name(a.ModuleBase.Name())
}

func (a *AARImport) DepsMutator(ctx android.BottomUpMutatorContext) {
	if !ctx.Config().UnbundledBuild() {
		sdkDep := decodeSdkDep(ctx, sdkContext(a))
		if sdkDep.useModule && sdkDep.frameworkResModule != "" {
			ctx.AddVariationDependencies(nil, frameworkResTag, sdkDep.frameworkResModule)
		}
	}

	ctx.AddVariationDependencies(nil, libTag, a.properties.Libs...)
	ctx.AddVariationDependencies(nil, staticLibTag, a.properties.Static_libs...)
}

// Unzip an AAR into its constituent files and directories.  Any files in Outputs that don't exist in the AAR will be
// touched to create an empty file, and any directories in $expectedDirs will be created.
var unzipAAR = pctx.AndroidStaticRule("unzipAAR",
	blueprint.RuleParams{
		Command: `rm -rf $outDir && mkdir -p $outDir $expectedDirs && ` +
			`unzip -qo -d $outDir $in && touch $out`,
	},
	"expectedDirs", "outDir")

func (a *AARImport) GenerateAndroidBuildActions(ctx android.ModuleContext) {
	if len(a.properties.Aars) != 1 {
		ctx.PropertyErrorf("aars", "exactly one aar is required")
		return
	}

	aarName := ctx.ModuleName() + ".aar"
	var aar android.Path
	aar = android.PathForModuleSrc(ctx, a.properties.Aars[0])
	if Bool(a.properties.Jetifier_enabled) {
		inputFile := aar
		aar = android.PathForModuleOut(ctx, "jetifier", aarName)
		TransformJetifier(ctx, aar.(android.WritablePath), inputFile)
	}

	extractedAARDir := android.PathForModuleOut(ctx, "aar")
	extractedResDir := extractedAARDir.Join(ctx, "res")
	a.classpathFile = extractedAARDir.Join(ctx, "classes.jar")
	a.proguardFlags = extractedAARDir.Join(ctx, "proguard.txt")
	a.manifest = extractedAARDir.Join(ctx, "AndroidManifest.xml")

	ctx.Build(pctx, android.BuildParams{
		Rule:        unzipAAR,
		Input:       aar,
		Outputs:     android.WritablePaths{a.classpathFile, a.proguardFlags, a.manifest},
		Description: "unzip AAR",
		Args: map[string]string{
			"expectedDirs": extractedResDir.String(),
			"outDir":       extractedAARDir.String(),
		},
	})

	compiledResDir := android.PathForModuleOut(ctx, "flat-res")
	aaptCompileDeps := android.Paths{a.classpathFile}
	aaptCompileDirs := android.Paths{extractedResDir}
	flata := compiledResDir.Join(ctx, "gen_res.flata")
	aapt2CompileDirs(ctx, flata, aaptCompileDirs, aaptCompileDeps)

	a.exportPackage = android.PathForModuleOut(ctx, "package-res.apk")
	srcJar := android.PathForModuleGen(ctx, "R.jar")
	proguardOptionsFile := android.PathForModuleGen(ctx, "proguard.options")
	rTxt := android.PathForModuleOut(ctx, "R.txt")
	a.extraAaptPackagesFile = android.PathForModuleOut(ctx, "extra_packages")

	var linkDeps android.Paths

	linkFlags := []string{
		"--static-lib",
		"--no-static-lib-packages",
		"--auto-add-overlay",
	}

	linkFlags = append(linkFlags, "--manifest "+a.manifest.String())
	linkDeps = append(linkDeps, a.manifest)

	transitiveStaticLibs, staticLibManifests, libDeps, libFlags := aaptLibs(ctx, sdkContext(a))

	_ = staticLibManifests

	linkDeps = append(linkDeps, libDeps...)
	linkFlags = append(linkFlags, libFlags...)

	overlayRes := append(android.Paths{flata}, transitiveStaticLibs...)

	aapt2Link(ctx, a.exportPackage, srcJar, proguardOptionsFile, rTxt, a.extraAaptPackagesFile,
		linkFlags, linkDeps, nil, overlayRes)
}

var _ Dependency = (*AARImport)(nil)

func (a *AARImport) HeaderJars() android.Paths {
	return android.Paths{a.classpathFile}
}

func (a *AARImport) ImplementationJars() android.Paths {
	return android.Paths{a.classpathFile}
}

func (a *AARImport) ResourceJars() android.Paths {
	return nil
}

func (a *AARImport) ImplementationAndResourcesJars() android.Paths {
	return android.Paths{a.classpathFile}
}

func (a *AARImport) AidlIncludeDirs() android.Paths {
	return nil
}

func (a *AARImport) ExportedSdkLibs() []string {
	return nil
}

var _ android.PrebuiltInterface = (*Import)(nil)

func AARImportFactory() android.Module {
	module := &AARImport{}

	module.AddProperties(&module.properties)

	android.InitPrebuiltModule(module, &module.properties.Aars)
	android.InitAndroidArchModule(module, android.DeviceSupported, android.MultilibCommon)
	return module
}
