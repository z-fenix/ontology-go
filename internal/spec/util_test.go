package spec

import "testing"

func TestToSnake(t *testing.T) {
	cases := map[string]string{
		"workOrderId": "work_order_id",
		"WorkOrder":   "work_order",
		"HTTPServer":  "http_server",
		"flightHours": "flight_hours",
		"id":          "id",
		"a1B":         "a1_b",
	}
	for in, want := range cases {
		if got := ToSnake(in); got != want {
			t.Errorf("ToSnake(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSuggest(t *testing.T) {
	cands := []string{"WorkOrderStatus", "TailNumber"}
	if got := suggest("WorkorderStatus", cands); got != "WorkOrderStatus" {
		t.Errorf("suggest = %q", got)
	}
	if got := suggest("TailNumbr", cands); got != "TailNumber" {
		t.Errorf("suggest = %q", got)
	}
	if got := suggest("Engine", cands); got != "" {
		t.Errorf("suggest = %q, want empty", got)
	}
}
