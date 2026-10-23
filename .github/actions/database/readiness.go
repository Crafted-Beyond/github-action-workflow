package main

import "reflect"

func databaseSourcesEqual(desired, compared map[string]any) bool {
	normalize := func(source map[string]any) map[string]any {
		result := make(map[string]any, len(source))
		for key, value := range source {
			result[key] = value
		}
		if helm, ok := source["helm"].(map[string]any); ok {
			copy := make(map[string]any, len(helm))
			for key, value := range helm {
				copy[key] = value
			}
			// Argo omits an empty Helm valueFiles list from comparedTo.source.
			if files, ok := copy["valueFiles"].([]any); ok && len(files) == 0 {
				delete(copy, "valueFiles")
			}
			result["helm"] = copy
		}
		return result
	}
	return reflect.DeepEqual(normalize(desired), normalize(compared))
}
