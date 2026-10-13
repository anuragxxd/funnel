// Package storageplan resolves native Kubernetes storage bindings for one task.
package storageplan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"reflect"
	"strings"
	"text/template"

	"github.com/ohsu-comp-bio/funnel/config"
	"github.com/ohsu-comp-bio/funnel/tes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	yamlreader "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

const (
	Annotation = "funnel.calypr.org/storage-plan"
	Env        = "FUNNEL_KUBERNETES_STORAGE_PLAN"
	Finalizer  = "funnel.calypr.org/storage-cleanup"
)

// Plan records the resolved mounts and the exact names of task-owned resources.
// Creation objects are excluded from the durable snapshot.
type Plan struct {
	Version           int
	TaskID            string
	Namespace         string
	WorkDir           string
	WorkspaceVolume   string
	WorkerContainer   string
	ExecutorContainer string
	MountedInputs     []*config.MountedInput
	Volumes           []corev1.Volume
	WorkerMounts      []corev1.VolumeMount
	OwnedClaims       []string
	OwnedVolumes      []string
	Claims            []*corev1.PersistentVolumeClaim `json:"-"`
	PersistentVolumes []*corev1.PersistentVolume      `json:"-"`
}

// Resolve returns nil for deployments using the legacy storage configuration.
func Resolve(task *tes.Task, conf *config.Config) (*Plan, error) {
	if conf == nil || conf.Kubernetes == nil || conf.Kubernetes.Storage == nil {
		return nil, nil
	}
	if task == nil || task.Id == "" {
		return nil, fmt.Errorf("storage requires a task ID")
	}
	ns := conf.Kubernetes.JobsNamespace
	if ns == "" {
		ns = conf.Kubernetes.Namespace
	}
	if errs := validation.IsDNS1123Label(ns); len(errs) != 0 {
		return nil, fmt.Errorf("invalid storage namespace %q: %s", ns, strings.Join(errs, ", "))
	}
	storage := conf.Kubernetes.Storage
	p := &Plan{
		Version:           1,
		TaskID:            task.Id,
		Namespace:         ns,
		WorkDir:           conf.GetWorker().GetWorkDir(),
		WorkspaceVolume:   storage.WorkspaceVolume,
		WorkerContainer:   storage.WorkerContainer,
		ExecutorContainer: storage.ExecutorContainer,
		MountedInputs:     conf.MountedInputs,
	}
	data := map[string]interface{}{"TaskId": task.Id, "Namespace": ns, "DiskGb": task.GetResources().GetDiskGb(), "Bucket": "", "Region": "", "KmsKeyID": ""}
	if len(conf.GenericS3) > 0 && conf.GenericS3[0] != nil {
		data["Bucket"], data["Region"], data["KmsKeyID"] = conf.GenericS3[0].Bucket, conf.GenericS3[0].Region, conf.GenericS3[0].KmsKeyID
	}
	for _, binding := range storage.Bindings {
		if binding == nil {
			return nil, fmt.Errorf("nil storage binding")
		}
		if errs := validation.IsDNS1123Label(binding.Name); len(errs) != 0 {
			return nil, fmt.Errorf("invalid storage binding name %q", binding.Name)
		}
		claimName := binding.GetExistingClaim()
		if templates := binding.GetTemplates(); templates != nil {
			if templates.PVC == "" {
				return nil, fmt.Errorf("binding %q requires a PVC template", binding.Name)
			}
			claim := &corev1.PersistentVolumeClaim{}
			if err := render(templates.PVC, data, claim); err != nil {
				return nil, fmt.Errorf("binding %q PVC: %w", binding.Name, err)
			}
			if claim.APIVersion != "v1" || claim.Kind != "PersistentVolumeClaim" {
				return nil, fmt.Errorf("binding %q template must be a v1 PersistentVolumeClaim", binding.Name)
			}
			if claim.Namespace == "" {
				claim.Namespace = ns
			}
			if claim.Namespace != ns {
				return nil, fmt.Errorf("binding %q PVC namespace must be %q", binding.Name, ns)
			}
			if len(claim.OwnerReferences) != 0 {
				return nil, fmt.Errorf("binding %q PVC must not contain owner references", binding.Name)
			}
			if claim.Spec.VolumeMode != nil && *claim.Spec.VolumeMode == corev1.PersistentVolumeBlock {
				return nil, fmt.Errorf("binding %q PVC must use filesystem volume mode", binding.Name)
			}
			if claim.GenerateName != "" {
				return nil, fmt.Errorf("binding %q requires an explicit PVC name", binding.Name)
			}
			if binding.Name == p.WorkspaceVolume && len(claim.Spec.AccessModes) > 0 {
				writable := false
				for _, mode := range claim.Spec.AccessModes {
					if mode == corev1.ReadWriteOncePod {
						return nil, fmt.Errorf("workspace PVC cannot use ReadWriteOncePod: worker and executor run in separate Pods")
					}
					if mode == corev1.ReadWriteOnce || mode == corev1.ReadWriteMany {
						writable = true
					}
				}
				if !writable {
					return nil, fmt.Errorf("workspace PVC must support writable access from worker and executor Pods")
				}
			}
			claimName = claim.Name
			if !strings.Contains(claimName, task.Id) {
				return nil, fmt.Errorf("binding %q PVC name must contain task ID %q", binding.Name, task.Id)
			}
			if templates.PV != "" {
				pv := &corev1.PersistentVolume{}
				if err := render(templates.PV, data, pv); err != nil {
					return nil, fmt.Errorf("binding %q PV: %w", binding.Name, err)
				}
				if pv.APIVersion != "v1" || pv.Kind != "PersistentVolume" {
					return nil, fmt.Errorf("binding %q template must be a v1 PersistentVolume", binding.Name)
				}
				if pv.Namespace != "" || pv.GenerateName != "" || !validName(pv.Name) || !strings.Contains(pv.Name, task.Id) {
					return nil, fmt.Errorf("binding %q requires a cluster-scoped PV with an explicit task-specific name", binding.Name)
				}
				if len(pv.OwnerReferences) != 0 {
					return nil, fmt.Errorf("binding %q PV must not contain owner references", binding.Name)
				}
				if pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode == corev1.PersistentVolumeBlock {
					return nil, fmt.Errorf("binding %q PV must use filesystem volume mode", binding.Name)
				}
				sources := reflect.ValueOf(pv.Spec.PersistentVolumeSource)
				count := 0
				for i := 0; i < sources.NumField(); i++ {
					if !sources.Field(i).IsNil() {
						count++
					}
				}
				if count != 1 {
					return nil, fmt.Errorf("binding %q PV must have exactly one native volume source", binding.Name)
				}
				if claim.Spec.VolumeName != "" && claim.Spec.VolumeName != pv.Name {
					return nil, fmt.Errorf("binding %q PVC volumeName conflicts with its PV", binding.Name)
				}
				claim.Spec.VolumeName = pv.Name
				if pv.Spec.ClaimRef == nil {
					pv.Spec.ClaimRef = &corev1.ObjectReference{}
				}
				ref := pv.Spec.ClaimRef
				if (ref.Name != "" && ref.Name != claim.Name) || (ref.Namespace != "" && ref.Namespace != ns) || ref.UID != "" || (ref.Kind != "" && ref.Kind != "PersistentVolumeClaim") || (ref.APIVersion != "" && ref.APIVersion != "v1") {
					return nil, fmt.Errorf("binding %q PV claimRef conflicts with its PVC", binding.Name)
				}
				ref.Name, ref.Namespace = claim.Name, ns
				p.PersistentVolumes = append(p.PersistentVolumes, pv)
				p.OwnedVolumes = append(p.OwnedVolumes, pv.Name)
			}
			p.Claims = append(p.Claims, claim)
			p.OwnedClaims = append(p.OwnedClaims, claim.Name)
		}
		if !validName(claimName) {
			return nil, fmt.Errorf("binding %q requires a valid claim name", binding.Name)
		}
		p.Volumes = append(p.Volumes, corev1.Volume{Name: binding.Name, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName}}})
		for _, raw := range binding.WorkerMounts {
			var mount corev1.VolumeMount
			if err := decodeNative([]byte(raw), &mount); err != nil {
				return nil, fmt.Errorf("binding %q worker mount: %w", binding.Name, err)
			}
			if mount.Name == "" {
				mount.Name = binding.Name
			}
			if mount.Name != binding.Name {
				return nil, fmt.Errorf("binding %q worker mount names a different volume", binding.Name)
			}
			p.WorkerMounts = append(p.WorkerMounts, mount)
		}
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func render(raw string, data map[string]interface{}, target interface{}) error {
	t, err := template.New("storage").Option("missingkey=error").Parse(raw)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return err
	}
	return decodeNative(buf.Bytes(), target)
}

func decodeNative(raw []byte, target interface{}) error {
	reader := yamlreader.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
	document, err := reader.Read()
	if err != nil {
		return err
	}
	for {
		extra, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if len(bytes.TrimSpace(extra)) != 0 {
			return fmt.Errorf("expected exactly one YAML object")
		}
	}
	data, err := yaml.YAMLToJSONStrict(document)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	return nil
}

func validName(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }

func (p *Plan) Encode() (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(p)
	return string(data), err
}

func Decode(snapshot string) (*Plan, error) {
	var p Plan
	decoder := json.NewDecoder(strings.NewReader(snapshot))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return nil, fmt.Errorf("decode storage plan: %w", err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("storage plan contains trailing data")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func (p *Plan) validate() error {
	if p == nil || p.Version != 1 {
		return fmt.Errorf("unsupported storage plan version")
	}
	if !validName(p.TaskID) {
		return fmt.Errorf("invalid storage task ID %q", p.TaskID)
	}
	if len(validation.IsDNS1123Label(p.Namespace)) != 0 {
		return fmt.Errorf("invalid storage namespace %q", p.Namespace)
	}
	if err := validateAbsolutePath(p.WorkDir); err != nil {
		return fmt.Errorf("storage work directory: %w", err)
	}
	volumes := make(map[string]corev1.Volume)
	claims := make(map[string]bool)
	for _, v := range p.Volumes {
		if len(validation.IsDNS1123Label(v.Name)) != 0 {
			return fmt.Errorf("invalid storage volume name %q", v.Name)
		}
		if _, exists := volumes[v.Name]; exists {
			return fmt.Errorf("duplicate storage binding %q", v.Name)
		}
		if v.PersistentVolumeClaim == nil || !validName(v.PersistentVolumeClaim.ClaimName) {
			return fmt.Errorf("storage volume %q must reference a valid PVC", v.Name)
		}
		// Snapshots may only contain PVC sources, never arbitrary pod volumes.
		expected := corev1.VolumeSource{PersistentVolumeClaim: v.PersistentVolumeClaim}
		if !reflect.DeepEqual(v.VolumeSource, expected) {
			return fmt.Errorf("storage volume %q has multiple sources", v.Name)
		}
		volumes[v.Name], claims[v.PersistentVolumeClaim.ClaimName] = v, true
	}
	workspace, exists := volumes[p.WorkspaceVolume]
	if !exists {
		return fmt.Errorf("workspace volume %q does not name a storage binding", p.WorkspaceVolume)
	}
	if workspace.PersistentVolumeClaim.ReadOnly {
		return fmt.Errorf("workspace volume must be writable")
	}
	for _, names := range [][]string{p.OwnedClaims, p.OwnedVolumes} {
		seen := make(map[string]bool)
		for _, name := range names {
			if !validName(name) || seen[name] || !strings.Contains(name, p.TaskID) {
				return fmt.Errorf("invalid or duplicate owned storage resource %q", name)
			}
			seen[name] = true
		}
	}
	if len(p.OwnedVolumes) > len(p.OwnedClaims) {
		return fmt.Errorf("owned persistent volumes require task-owned claims")
	}
	for _, claim := range p.OwnedClaims {
		if !claims[claim] {
			return fmt.Errorf("owned claim %q is not mounted by a storage binding", claim)
		}
	}
	paths := []string{path.Join(p.WorkDir, p.TaskID)}
	for _, mount := range p.WorkerMounts {
		if _, ok := volumes[mount.Name]; !ok {
			return fmt.Errorf("worker mount references unknown volume %q", mount.Name)
		}
		if err := validateMount(mount); err != nil {
			return err
		}
		for _, existing := range paths {
			if pathsOverlap(existing, mount.MountPath) {
				return fmt.Errorf("worker mount %q overlaps mount %q", mount.MountPath, existing)
			}
		}
		paths = append(paths, mount.MountPath)
	}
	return nil
}
