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

// goKinds gives the manifest kind of every Go repo of tasks-go.jsonl; unknown repos default to "library". Fixed before looking at any
// result: applications ship a binary or service (CLIs, servers, operators, agents), frameworks are what other programs build on and
// extend (web/rpc/controller frameworks), everything else is a library.
var goKinds = map[string]string{
	"0xerr0r/blocky": "application", "gentleman-programming/gentle-ai": "application", "ibm/sarama": "library",
	"nvidia/gpu-operator": "application", "techarohq/anubis": "application", "a-h/templ": "framework", "akuity/kargo": "application",
	"amir20/dozzle": "application", "argoproj/argo-rollouts": "application", "bpg/terraform-provider-proxmox": "application",
	"caddyserver/caddy": "application", "cadence-workflow/cadence": "application", "casbin/casbin": "library",
	"centrifugal/centrifugo": "application", "cert-manager/cert-manager": "application", "charmbracelet/crush": "application",
	"chenhg5/cc-connect": "application", "cli/cli": "application", "coder/coder": "application", "coze-dev/coze-loop": "application",
	"crowdsecurity/crowdsec": "application", "cucumber/godog": "framework", "danielmiessler/fabric": "application",
	"dlvhdr/gh-dash": "application", "envoyproxy/gateway": "application", "ethereum/go-ethereum": "application", "evcc-io/evcc": "application",
	"fortio/fortio": "application", "gastownhall/beads": "application", "getgauge/gauge": "framework", "gin-gonic/gin": "framework",
	"github/gh-aw": "application", "github/github-mcp-server": "application", "go-task/task": "application",
	"go-vikunja/vikunja": "application", "gohugoio/hugo": "application", "golang-jwt/jwt": "library", "google/go-github": "library",
	"google/starlark-go": "library", "graphql-go/graphql": "library", "grpc/grpc-go": "framework", "gruntwork-io/cloud-nuke": "application",
	"hashicorp/terraform": "application", "influxdata/telegraf": "application", "istio/istio": "application",
	"jesseduffield/lazygit": "application", "karmada-io/karmada": "application", "kedacore/keda": "application",
	"knative/eventing": "framework", "kube-vip/kube-vip": "application", "kubernetes-sigs/controller-runtime": "framework",
	"kubernetes-sigs/kubebuilder": "framework", "kubernetes-sigs/kueue": "application", "kubernetes/kops": "application",
	"kubernetes/kube-state-metrics": "application", "kumahq/kuma": "application", "kyverno/kyverno": "application",
	"libp2p/go-libp2p": "framework", "lima-vm/lima": "application", "magefile/mage": "application", "mgechev/revive": "application",
	"moby/moby": "application", "modelcontextprotocol/go-sdk": "library", "modelcontextprotocol/registry": "application",
	"moond4rk/hackbrowserdata": "application", "nats-io/nats-server": "application", "netbirdio/netbird": "application",
	"nextlevelbuilder/goclaw": "application", "nginx/kubernetes-ingress": "application", "oauth2-proxy/oauth2-proxy": "application",
	"ollama/ollama": "application", "open-telemetry/opentelemetry-collector-contrib": "application",
	"open-telemetry/opentelemetry-ebpf-profiler": "application", "open-telemetry/opentelemetry-go": "library",
	"open-telemetry/opentelemetry-go-contrib": "library", "open-telemetry/opentelemetry-operator": "application",
	"openfga/openfga": "application", "ossf/scorecard": "application", "pelletier/go-toml": "library",
	"prometheus/alertmanager": "application", "prometheus/prometheus": "application", "redpanda-data/connect": "application",
	"rook/rook": "application", "rqlite/rqlite": "application", "slack-go/slack": "library", "sqlc-dev/sqlc": "application",
	"stripe/stripe-cli": "application", "submariner-io/submariner": "application", "sustainable-computing-io/kepler": "application",
	"terraform-docs/terraform-docs": "application", "tikv/pd": "application", "tmc/langchaingo": "library", "twpayne/chezmoi": "application",
	"velero-io/velero": "application", "yuin/gopher-lua": "library", "zeromicro/go-zero": "framework", "zitadel/zitadel": "application",
}

// GoRepoKind is the manifest kind of a Go repo.
func GoRepoKind(repo string) string { return kindOf(goKinds, repo) }

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
	if eco == "go" {
		kind = GoRepoKind
		ds = []DatasetInfo{
			{Name: "SWE-bench/SWE-bench_Multilingual", URL: "https://huggingface.co/datasets/SWE-bench/SWE-bench_Multilingual", Notes: "test split; keeps Go repos (instances whose test patch touches a _test.go file)"},
			{Name: "ByteDance-Seed/Multi-SWE-bench", URL: "https://huggingface.co/datasets/ByteDance-Seed/Multi-SWE-bench", Notes: "go/ directory (JSONL); FAIL_TO_PASS = f2p_tests keys, else n2p_tests keys (task field f2p_kind)"},
			{Name: "SWE-bench-Live/MultiLang", URL: "https://huggingface.co/datasets/SWE-bench-Live/MultiLang", Notes: "split go; FAIL_TO_PASS names are <import path>/<TestName>"},
		}
	} else if eco == "dotnet" {
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
