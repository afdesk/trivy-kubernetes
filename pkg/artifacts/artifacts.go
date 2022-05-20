package artifacts

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/aquasecurity/trivy-kubernetes/pkg/k8s"
)

// Artifact holds information for kubernetes scannable resources
type Artifact struct {
	Namespace   string
	Kind        string
	Name        string
	Images      []string
	RawResource map[string]interface{}
}

// FromResource is a factory method to create an Artifact from an unstructured.Unstructured
func FromResource(resource unstructured.Unstructured) (*Artifact, error) {
	var nestedKeys []string

	switch resource.GetKind() {
	case k8s.KindPod:
		nestedKeys = []string{"spec"}
	case k8s.KindCronJob:
		nestedKeys = []string{"spec", "jobTemplate", "spec", "template", "spec"}
	default:
		nestedKeys = []string{"spec", "template", "spec"}
	}

	images := make([]string, 0)

	containersImages, err := extractImages(resource, append(nestedKeys, "containers"))
	if err != nil {
		return nil, err
	}
	images = append(images, containersImages...)

	ephemeralContainersImages, err := extractImages(resource, append(nestedKeys, "ephemeralContainers"))
	if err != nil {
		return nil, err
	}
	images = append(images, ephemeralContainersImages...)

	initContainersImages, err := extractImages(resource, append(nestedKeys, "initContainers"))
	if err != nil {
		return nil, err
	}
	images = append(images, initContainersImages...)

	// we don't check found here, if the name is not found it will be an empty string
	name, _, err := unstructured.NestedString(resource.Object, "metadata", "name")
	if err != nil {
		return nil, err
	}

	return &Artifact{
		Namespace:   resource.GetNamespace(),
		Kind:        resource.GetKind(),
		Name:        name,
		Images:      images,
		RawResource: resource.Object,
	}, nil
}

func extractImages(resource unstructured.Unstructured, keys []string) ([]string, error) {
	containers, found, err := unstructured.NestedSlice(resource.Object, keys...)
	if err != nil {
		return []string{}, err
	}

	if !found {
		return []string{}, nil
	}

	images := make([]string, 0)
	for _, container := range containers {
		name, found, err := unstructured.NestedString(container.(map[string]interface{}), "image")
		if err != nil {
			return []string{}, err
		}

		if found {
			images = append(images, name)
		}
	}

	return images, nil
}
