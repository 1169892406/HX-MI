package main

func extractMIoTBool(rows []map[string]any) (bool, bool) {
	if len(rows) == 0 {
		return false, false
	}
	if code, ok := rows[0]["code"].(float64); ok && code != 0 {
		return false, false
	}
	v, ok := rows[0]["value"].(bool)
	return v, ok
}
