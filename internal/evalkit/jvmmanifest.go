package evalkit

import (
	"encoding/json"
	"os"
	"strings"
)

// jvmKinds and dotnetKinds give the manifest kind (library | framework | application; plan section 6.2 reports selectivity per kind) of
// every repo in tasks-jvm.jsonl / tasks-dotnet.jsonl. Unknown repos default to "library". Fixed before looking at any result.
var jvmKinds = map[string]string{
	"gradleup/shadow": "library", "hannah-sten/texify-idea": "application", "kotlin/dataframe": "library",
	"reactivex/rxjava": "library", "alibaba/fastjson2": "library", "ankidroid/anki-android": "application",
	"apache/druid": "application", "apache/dubbo": "framework", "apache/lucene": "library", "detekt/detekt": "application",
	"elastic/logstash": "application", "fasterxml/jackson-core": "library", "fasterxml/jackson-databind": "library",
	"fasterxml/jackson-dataformat-xml": "library", "google/gson": "library", "googlecontainertools/jib": "application",
	"javaparser/javaparser": "library", "mockito/mockito": "library", "oss-review-toolkit/ort": "application",
	"pinterest/ktlint": "application", "projectlombok/lombok": "library", "square/okhttp": "library",
}

var dotnetKinds = map[string]string{
	"avaloniaui/avalonia": "framework", "dotnet/efcore": "framework", "dotnet/aspnetcore": "framework", "dotnet/orleans": "framework",
	"dotnet/winforms": "framework", "unoplatform/uno": "framework", "elsa-workflows/elsa-core": "framework", "jasperfx/wolverine": "framework",
	"threemammals/ocelot": "framework", "orchardcms/orchardcore": "framework", "dotnet/yarp": "application", "dotnet/sdk": "application",
	"jellyfin/jellyfin": "application", "kareadita/kavita": "application", "ombi-app/ombi": "application", "ppy/osu": "application",
	"subtitleedit/subtitleedit": "application", "ksp-ckan/ckan": "application", "ardalis/cleanarchitecture": "application",
	"azure/azure-mcp": "application", "erikej/efcorepowertools": "application", "flow-launcher/flow.launcher": "application",
	"openclaw/openclaw-windows-node": "application",
}

// JVMRepoKind is the manifest kind of a JVM repo.
func JVMRepoKind(repo string) string { return kindOf(jvmKinds, repo) }

// DotnetRepoKind is the manifest kind of a .NET repo.
func DotnetRepoKind(repo string) string { return kindOf(dotnetKinds, repo) }

func kindOf(m map[string]string, repo string) string {
	if k, ok := m[strings.ToLower(repo)]; ok {
		return k
	}
	return "library"
}

// BuildJVMDotnetManifest builds the deterministic manifest of "jvm" or "dotnet" (BuildCountManifest split rule, kinds from the tables above).
// metaPath (optional) supplies the dataset revisions recorded by fetch_jvm.py / fetch_dotnet.py.
func BuildJVMDotnetManifest(eco string, t1 map[string]*ManifestRepo, metaPath string, capPer int) Manifest {
	kind := JVMRepoKind
	var ds []DatasetInfo
	if eco == "dotnet" {
		kind = DotnetRepoKind
		ds = []DatasetInfo{
			{Name: "microsoft/SWE-Sharp-Bench", URL: "https://huggingface.co/datasets/microsoft/SWE-Sharp-Bench", Notes: "swe-sharp-bench.csv (150 tasks, 17 repos); FAIL_TO_PASS is a Python list literal; tasks with an empty FAIL_TO_PASS dropped"},
			{Name: "SWE-bench-Live/MultiLang", URL: "https://huggingface.co/datasets/SWE-bench-Live/MultiLang", Notes: "split cs (136 tasks, 41 repos)"},
			{Name: "SWE-bench-Live/Windows", URL: "https://huggingface.co/datasets/SWE-bench-Live/Windows", Notes: "test split, rows with language C# only"},
		}
	} else {
		ds = []DatasetInfo{
			{Name: "SWE-bench/SWE-bench_Multilingual", URL: "https://huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual", Notes: "test split; keeps Java repos (RxJava, druid, lucene, gson, javaparser, lombok)"},
			{Name: "ByteDance-Seed/Multi-SWE-bench", URL: "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench", Notes: "java/ and kotlin/ directories (JSONL); FAIL_TO_PASS = f2p_tests keys, else n2p_tests keys (task field f2p_kind)"},
		}
	}
	if b, err := os.ReadFile(metaPath); err == nil {
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) == nil {
			var revs map[string]string
			if json.Unmarshal(m[eco+"_datasets"], &revs) == nil {
				for i := range ds {
					ds[i].Revision = revs[ds[i].Name]
				}
			}
		}
	}
	for _, r := range t1 {
		if r.Kind == "" {
			r.Kind = kind(r.Name)
		}
	}
	return BuildCountManifest(eco, t1, ds, capPer)
}
