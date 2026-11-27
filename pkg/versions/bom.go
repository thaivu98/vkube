package versions

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/thaivu98/vkube/pkg/api"
	"gopkg.in/yaml.v3"
)

//go:embed versions.yaml
var defaultBOMYAML []byte

const (
	ArtifactTypeBinary   = "binary"
	ArtifactTypeTarball  = "tarball"
	ArtifactTypeManifest = "manifest"
)

// BOM (Bill of Materials) khai báo toàn bộ version, URL template và OCI images từ file versions.yaml.
type BOM struct {
	ReleaseTag  string            `yaml:"releaseTag" json:"releaseTag"`
	Components  map[string]string `yaml:"components" json:"components"`
	Artifacts   []ArtifactSpec    `yaml:"artifacts" json:"artifacts"`
	ImageGroups []ImageGroupSpec  `yaml:"imageGroups" json:"imageGroups"`
}

// ArtifactSpec mô tả một artifact cần tải (binary, tarball hoặc manifest) với URL dạng Go template.
type ArtifactSpec struct {
	Name      string `yaml:"name" json:"name"`
	Component string `yaml:"component" json:"component"`
	Type      string `yaml:"type" json:"type"`
	URL       string `yaml:"url" json:"url"`
	SHA256    string `yaml:"sha256,omitempty" json:"sha256,omitempty"`
	SHA256URL string `yaml:"sha256Url,omitempty" json:"sha256Url,omitempty"`
	Dest      string `yaml:"dest" json:"dest"`
}

// ImageGroupSpec nhóm các OCI images theo từng file tar đầu ra trong Offline Bundle.
type ImageGroupSpec struct {
	Name   string   `yaml:"name" json:"name"`
	Dest   string   `yaml:"dest" json:"dest"`
	Images []string `yaml:"images" json:"images"`
}

// ResolvedBOM chứa toàn bộ URL và danh sách OCI images đã render cụ thể cho một kiến trúc đích.
type ResolvedBOM struct {
	ReleaseTag        string
	KubernetesVersion string
	Arch              string
	Components        map[string]string
	Artifacts         []ResolvedArtifact
	ImageGroups       []ResolvedImageGroup
}

// ResolvedArtifact là artifact đã được thay thế {{.Version}} và {{.Arch}} thành URL thực tế.
type ResolvedArtifact struct {
	Name      string
	Component string
	Version   string
	Type      string
	URL       string
	SHA256    string
	SHA256URL string
	Dest      string
}

// ResolvedImageGroup là nhóm OCI images đã render tag phiên bản đầy đủ.
type ResolvedImageGroup struct {
	Name   string
	Dest   string
	Images []string
}

// DefaultBOMBytes trả về bản sao nội dung YAML của file BOM mặc định được nhúng trong binary.
func DefaultBOMBytes() []byte {
	out := make([]byte, len(defaultBOMYAML))
	copy(out, defaultBOMYAML)
	return out
}

// Load đọc BOM từ file ngoài nếu manifestPath được chỉ định (--manifest), ngược lại dùng BOM mặc định nhúng sẵn.
func Load(manifestPath string) (*BOM, error) {
	if strings.TrimSpace(manifestPath) == "" {
		return LoadDefault()
	}
	return LoadFromFile(manifestPath)
}

// LoadDefault parse file versions.yaml mặc định được nhúng qua go:embed.
func LoadDefault() (*BOM, error) {
	return Parse(defaultBOMYAML)
}

// LoadFromFile đọc và parse một file BOM tùy chỉnh do người dùng cung cấp qua --manifest.
func LoadFromFile(filePath string) (*BOM, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read BOM manifest %q: %w", filePath, err)
	}
	return Parse(data)
}

// Parse giải mã nội dung YAML của BOM và kiểm tra cấu trúc tối thiểu.
func Parse(data []byte) (*BOM, error) {
	var bom BOM
	if err := yaml.Unmarshal(data, &bom); err != nil {
		return nil, fmt.Errorf("parse BOM YAML: %w", err)
	}
	if err := bom.Validate(); err != nil {
		return nil, err
	}
	return &bom, nil
}

// Validate kiểm tra tính đầy đủ của BOM trước khi render URL/images.
func (b *BOM) Validate() error {
	if strings.TrimSpace(b.ReleaseTag) == "" {
		return fmt.Errorf("BOM releaseTag is required")
	}
	if len(b.Components) == 0 {
		return fmt.Errorf("BOM components map cannot be empty")
	}
	if strings.TrimSpace(b.Components["kubernetes"]) == "" {
		return fmt.Errorf("BOM components.kubernetes is required")
	}
	if len(b.Artifacts) == 0 {
		return fmt.Errorf("BOM artifacts list cannot be empty")
	}
	for i, a := range b.Artifacts {
		if a.Name == "" || a.Component == "" || a.URL == "" || a.Dest == "" {
			return fmt.Errorf("BOM artifacts[%d] (%q) must specify name, component, url, and dest", i, a.Name)
		}
		if _, ok := b.Components[a.Component]; !ok {
			return fmt.Errorf("BOM artifact %q references unknown component %q", a.Name, a.Component)
		}
		switch a.Type {
		case ArtifactTypeBinary, ArtifactTypeTarball, ArtifactTypeManifest:
		default:
			return fmt.Errorf("BOM artifact %q has unsupported type %q", a.Name, a.Type)
		}
	}
	for i, g := range b.ImageGroups {
		if g.Name == "" || g.Dest == "" || len(g.Images) == 0 {
			return fmt.Errorf("BOM imageGroups[%d] (%q) must specify name, dest, and at least one image", i, g.Name)
		}
	}
	return nil
}

// Resolve render toàn bộ URL tải artifact và danh sách OCI image cho kiến trúc đích (amd64 hoặc arm64).
func (b *BOM) Resolve(arch string) (*ResolvedBOM, error) {
	if err := api.ValidateArch(arch); err != nil {
		return nil, err
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}

	componentsCopy := make(map[string]string, len(b.Components))
	for k, v := range b.Components {
		componentsCopy[k] = v
	}

	resolved := &ResolvedBOM{
		ReleaseTag:        b.ReleaseTag,
		KubernetesVersion: b.Components["kubernetes"],
		Arch:              arch,
		Components:        componentsCopy,
		Artifacts:         make([]ResolvedArtifact, 0, len(b.Artifacts)),
		ImageGroups:       make([]ResolvedImageGroup, 0, len(b.ImageGroups)),
	}

	for _, a := range b.Artifacts {
		ver := b.Components[a.Component]
		ctx := map[string]any{
			"Version":    ver,
			"Arch":       arch,
			"Components": b.Components,
		}
		urlStr, err := renderTemplate("artifact-url:"+a.Name, a.URL, ctx)
		if err != nil {
			return nil, err
		}
		var shaURL string
		if a.SHA256URL != "" {
			shaURL, err = renderTemplate("artifact-sha256:"+a.Name, a.SHA256URL, ctx)
			if err != nil {
				return nil, err
			}
		}
		resolved.Artifacts = append(resolved.Artifacts, ResolvedArtifact{
			Name:      a.Name,
			Component: a.Component,
			Version:   ver,
			Type:      a.Type,
			URL:       urlStr,
			SHA256:    strings.ToLower(strings.TrimSpace(a.SHA256)),
			SHA256URL: shaURL,
			Dest:      a.Dest,
		})
	}

	imgCtx := map[string]any{
		"Arch":       arch,
		"Components": b.Components,
	}
	for _, g := range b.ImageGroups {
		imgs := make([]string, 0, len(g.Images))
		for _, rawImg := range g.Images {
			rendered, err := renderTemplate("image:"+g.Name, rawImg, imgCtx)
			if err != nil {
				return nil, err
			}
			imgs = append(imgs, rendered)
		}
		resolved.ImageGroups = append(resolved.ImageGroups, ResolvedImageGroup{
			Name:   g.Name,
			Dest:   g.Dest,
			Images: imgs,
		})
	}

	return resolved, nil
}

func renderTemplate(name, tmplStr string, data any) (string, error) {
	t, err := template.New(name).Option("missingkey=error").Parse(tmplStr)
	if err != nil {
		return "", fmt.Errorf("parse template %s: %w", name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render template %s: %w", name, err)
	}
	return buf.String(), nil
}
