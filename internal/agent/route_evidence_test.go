package agent

import "testing"

func TestServingProcessStartReadsSystemctlShow(t *testing.T) {
	for name, tc := range map[string]struct {
		show    string
		want    string
		wantErr bool
	}{
		"running":   {show: "LoadState=loaded\nActiveState=active\nMainPID=812\nExecMainStartTimestampMonotonic=4022331\n", want: "4022331"},
		"stopped":   {show: "LoadState=loaded\nActiveState=inactive\nMainPID=0\nExecMainStartTimestampMonotonic=4022331\n", want: ""},
		"failed":    {show: "LoadState=loaded\nActiveState=failed\nMainPID=0\nExecMainStartTimestampMonotonic=0\n", want: ""},
		"not found": {show: "LoadState=not-found\nActiveState=inactive\nMainPID=0\nExecMainStartTimestampMonotonic=0\n", wantErr: true},
		"starting":  {show: "LoadState=loaded\nActiveState=activating\nMainPID=0\nExecMainStartTimestampMonotonic=0\n", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := servingProcessStart("traefik.service", tc.show)
			if (err != nil) != tc.wantErr || got != tc.want {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}
