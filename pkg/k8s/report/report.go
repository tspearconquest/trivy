package report

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slices"
	"golang.org/x/xerrors"

	dbTypes "github.com/aquasecurity/trivy-db/pkg/types"
	"github.com/aquasecurity/trivy-kubernetes/pkg/artifacts"
	ftypes "github.com/aquasecurity/trivy/pkg/fanal/types"
	"github.com/aquasecurity/trivy/pkg/log"
	"github.com/aquasecurity/trivy/pkg/report/table"
	"github.com/aquasecurity/trivy/pkg/types"
)

const (
	allReport     = "all"
	summaryReport = "summary"

	tableFormat = "table"
	jsonFormat  = "json"

	workloadComponent = "workload"
	infraComponent    = "infra"
)

type Option struct {
	Format        string
	Report        string
	Output        io.Writer
	Severities    []dbTypes.Severity
	ColumnHeading []string
	Scanners      []string
	Components    []string
}

// Report represents a kubernetes scan report
type Report struct {
	SchemaVersion     int `json:",omitempty"`
	ClusterName       string
	Vulnerabilities   []Resource `json:",omitempty"`
	Misconfigurations []Resource `json:",omitempty"`
	name              string
}

// ConsolidatedReport represents a kubernetes scan report with consolidated findings
type ConsolidatedReport struct {
	SchemaVersion int `json:",omitempty"`
	ClusterName   string
	Findings      []Resource `json:",omitempty"`
}

// Resource represents a kubernetes resource report
type Resource struct {
	Namespace string `json:",omitempty"`
	Kind      string
	Name      string
	// TODO(josedonizetti): should add metadata? per report? per Result?
	// Metadata  Metadata `json:",omitempty"`
	Results types.Results `json:",omitempty"`
	Error   string        `json:",omitempty"`

	// original report
	Report types.Report `json:"-"`
}

func (r Resource) fullname() string {
	return strings.ToLower(fmt.Sprintf("%s/%s/%s", r.Namespace, r.Kind, r.Name))
}

// Failed returns whether the k8s report includes any vulnerabilities or misconfigurations
func (r Report) Failed() bool {
	for _, v := range r.Vulnerabilities {
		if v.Results.Failed() {
			return true
		}
	}

	for _, m := range r.Misconfigurations {
		if m.Results.Failed() {
			return true
		}
	}

	return false
}

func (r Report) consolidate() ConsolidatedReport {
	consolidated := ConsolidatedReport{
		SchemaVersion: r.SchemaVersion,
		ClusterName:   r.ClusterName,
	}

	index := make(map[string]Resource)

	for _, m := range r.Misconfigurations {
		index[m.fullname()] = m
	}

	for _, v := range r.Vulnerabilities {
		key := v.fullname()

		if res, ok := index[key]; ok {
			index[key] = Resource{
				Namespace: res.Namespace,
				Kind:      res.Kind,
				Name:      res.Name,
				Results:   append(res.Results, v.Results...),
				Error:     res.Error,
			}

			continue
		}

		index[key] = v
	}

	consolidated.Findings = maps.Values(index)

	return consolidated
}

// Writer defines the result write operation
type Writer interface {
	Write(Report) error
}

// Write writes the results in the give format
func Write(report Report, option Option) error {
	report.printErrors()

	switch option.Format {
	case jsonFormat:
		jwriter := JSONWriter{
			Output: option.Output,
			Report: option.Report,
		}
		return jwriter.Write(report)
	case tableFormat:
		separatedReports := separateMisconfigReports(report, option.Scanners, option.Components)

		if option.Report == summaryReport {
			target := fmt.Sprintf("Summary Report for %s", report.ClusterName)
			table.RenderTarget(option.Output, target, table.IsOutputToTerminal(option.Output))
		}

		for _, r := range separatedReports {
			writer := &TableWriter{
				Output:        option.Output,
				Report:        option.Report,
				Severities:    option.Severities,
				ColumnHeading: ColumnHeading(option.Scanners, option.Components, r.columns),
			}

			if err := writer.Write(r.report); err != nil {
				return err
			}
		}

		return nil
	default:
		return xerrors.Errorf(`unknown format %q. Use "json" or "table"`, option.Format)
	}
}

type reports struct {
	report  Report
	columns []string
}

// separateMisconfigReports returns 3 reports based on scanners and components flags,
// - misconfiguration report
// - rbac report
// - infra checks report
func separateMisconfigReports(k8sReport Report, scanners, components []string) []reports {

	workloadMisconfig := make([]Resource, 0)
	infraMisconfig := make([]Resource, 0)
	rbacAssessment := make([]Resource, 0)

	for _, misConfig := range k8sReport.Misconfigurations {
		switch {
		case slices.Contains(scanners, types.RBACScanner) && rbacResource(misConfig):
			rbacAssessment = append(rbacAssessment, misConfig)
		case infraResource(misConfig):
			workload, infra := splitInfraAndWorkloadResources(misConfig)

			if slices.Contains(components, infraComponent) {
				infraMisconfig = append(infraMisconfig, infra)
			}

			if slices.Contains(components, workloadComponent) {
				workloadMisconfig = append(workloadMisconfig, workload)
			}

		case slices.Contains(scanners, types.MisconfigScanner) && !rbacResource(misConfig):
			if slices.Contains(components, workloadComponent) {
				workloadMisconfig = append(workloadMisconfig, misConfig)
			}
		}
	}

	r := make([]reports, 0)

	if shouldAddWorkloadReport(scanners) {
		workloadReport := Report{
			SchemaVersion:     0,
			ClusterName:       k8sReport.ClusterName,
			Misconfigurations: workloadMisconfig,
			Vulnerabilities:   k8sReport.Vulnerabilities,
			name:              "Workload Assessment",
		}

		if (slices.Contains(components, workloadComponent) &&
			len(workloadMisconfig) > 0) ||
			len(k8sReport.Vulnerabilities) > 0 {
			r = append(r, reports{
				report:  workloadReport,
				columns: WorkloadColumns(),
			})
		}
	}

	if slices.Contains(scanners, types.RBACScanner) && len(rbacAssessment) > 0 {
		r = append(r, reports{
			report: Report{
				SchemaVersion:     0,
				ClusterName:       k8sReport.ClusterName,
				Misconfigurations: rbacAssessment,
				name:              "RBAC Assessment",
			},
			columns: RoleColumns(),
		})
	}

	if slices.Contains(scanners, types.MisconfigScanner) &&
		slices.Contains(components, infraComponent) &&
		len(infraMisconfig) > 0 {

		r = append(r, reports{
			report: Report{
				SchemaVersion:     0,
				ClusterName:       k8sReport.ClusterName,
				Misconfigurations: infraMisconfig,
				name:              "Infra Assessment",
			},
			columns: InfraColumns(),
		})
	}

	return r
}

func rbacResource(misConfig Resource) bool {
	return misConfig.Kind == "Role" || misConfig.Kind == "RoleBinding" || misConfig.Kind == "ClusterRole" || misConfig.Kind == "ClusterRoleBinding"
}

func infraResource(misConfig Resource) bool {
	return misConfig.Kind == "Pod" && misConfig.Namespace == "kube-system"
}

func CreateResource(artifact *artifacts.Artifact, report types.Report, err error) Resource {
	results := make([]types.Result, 0, len(report.Results))
	// fix target name
	for _, result := range report.Results {
		// if resource is a kubernetes file fix the target name,
		// to avoid showing the temp file that was removed.
		if result.Type == ftypes.Kubernetes {
			result.Target = fmt.Sprintf("%s/%s", artifact.Kind, artifact.Name)
		}
		results = append(results, result)
	}

	r := Resource{
		Namespace: artifact.Namespace,
		Kind:      artifact.Kind,
		Name:      artifact.Name,
		Results:   results,
		Report:    report,
	}

	// if there was any error during the scan
	if err != nil {
		r.Error = err.Error()
	}

	return r
}

func (r Report) printErrors() {
	for _, resource := range r.Vulnerabilities {
		if resource.Error != "" {
			log.Logger.Errorf("Error during vulnerabilities scan: %s", resource.Error)
		}
	}

	for _, resource := range r.Misconfigurations {
		if resource.Error != "" {
			log.Logger.Errorf("Error during misconfiguration scan: %s", resource.Error)
		}
	}
}

func splitInfraAndWorkloadResources(misconfig Resource) (Resource, Resource) {
	workload := copyResource(misconfig)
	infra := copyResource(misconfig)

	workloadResults := make(types.Results, 0)
	infraResults := make(types.Results, 0)

	for _, result := range misconfig.Results {
		workloadMisconfigs := make([]types.DetectedMisconfiguration, 0)
		infraMisconfigs := make([]types.DetectedMisconfiguration, 0)

		for _, m := range result.Misconfigurations {
			if strings.HasPrefix(m.ID, "KCV") {
				infraMisconfigs = append(infraMisconfigs, m)
				continue
			}

			workloadMisconfigs = append(workloadMisconfigs, m)
		}

		if len(workloadMisconfigs) > 0 {
			workloadResults = append(workloadResults, copyResult(result, workloadMisconfigs))
		}

		if len(infraMisconfigs) > 0 {
			infraResults = append(infraResults, copyResult(result, infraMisconfigs))
		}
	}

	workload.Results = workloadResults
	workload.Report.Results = workloadResults

	infra.Results = infraResults
	infra.Report.Results = infraResults

	return workload, infra
}

func copyResource(r Resource) Resource {
	return Resource{
		Namespace: r.Namespace,
		Kind:      r.Kind,
		Name:      r.Name,
		Error:     r.Error,
		Report:    r.Report,
	}
}

func copyResult(r types.Result, misconfigs []types.DetectedMisconfiguration) types.Result {
	return types.Result{
		Target:            r.Target,
		Class:             r.Class,
		Type:              r.Type,
		MisconfSummary:    r.MisconfSummary,
		Misconfigurations: misconfigs,
	}
}

func shouldAddWorkloadReport(scanners []string) bool {
	return slices.Contains(scanners, types.MisconfigScanner) ||
		slices.Contains(scanners, types.VulnerabilityScanner) ||
		slices.Contains(scanners, types.SecretScanner)
}
