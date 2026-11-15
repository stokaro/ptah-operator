// Package kubeletenv is the environment the kubelet gives a container's
// process, for tests that hold a built Job to the values its process reads
// rather than to the values the Job declares. The two differ: the kubelet
// expands $(VAR) and reduces $$ in every literal value before the process
// starts.
package kubeletenv

import corev1 "k8s.io/api/core/v1"

// Source gives the value a ValueFrom variable reads, and false for an
// optional one the kubelet leaves out.
type Source func(corev1.EnvVar) (string, bool, error)

// Resolve is env as a container's process reads it, one entry for each
// variable the kubelet sets, in the order declared, each carrying in Value
// what the process reads. It follows makeEnvironmentVariables in
// pkg/kubelet/kubelet_pods.go at v1.36.4: a literal value that is not empty
// is expanded against the variables resolved before it, and a value read from
// a source is taken as it is. ValueFrom is kept, so a caller can still tell a
// Secret's value from a literal. The container is taken to declare no envFrom
// and to run with service links off, as the operator's Jobs do, so nothing
// else can be referenced.
func Resolve(env []corev1.EnvVar, source Source) ([]corev1.EnvVar, error) {
	resolved := make([]corev1.EnvVar, 0, len(env))
	seen := make(map[string]string, len(env))
	mapping := MappingFuncFor(seen)
	for _, variable := range env {
		value := variable.Value
		switch {
		case value != "":
			value = Expand(value, mapping)
		case variable.ValueFrom != nil:
			read, present, err := source(variable)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			value = read
		}
		seen[variable.Name] = value
		variable.Value = value
		resolved = append(resolved, variable)
	}
	return resolved, nil
}

// Values is what Resolve returns, by name: a later variable of a name
// replaces an earlier one, as it does in the process's environment.
func Values(env []corev1.EnvVar, source Source) (map[string]string, error) {
	resolved, err := Resolve(env, source)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(resolved))
	for _, variable := range resolved {
		values[variable.Name] = variable.Value
	}
	return values, nil
}
