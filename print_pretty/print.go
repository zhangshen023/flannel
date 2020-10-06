package print_pretty

import "encoding/json"

func PrettyPrint(v interface{}) string {
	bytes, _ := json.Marshal(v)
	return string(bytes)
}
