package docsync

import "testing"

func TestCanonical(t *testing.T) {
	tests := []struct {
		name string
		file string
		in   string
		want string
	}{
		{
			name: "json keys are sorted and whitespace removed",
			file: "x.json",
			in:   "{\n  \"b\": 2,\n  \"a\": 1\n}\n",
			want: `{"a":1,"b":2}`,
		},
		{
			// float64 would round this to 9007199254740993 -> 9007199254740992.
			name: "large integers keep their exact value",
			file: "x.json",
			in:   `{"id":9007199254740993}`,
			want: `{"id":9007199254740993}`,
		},
		{
			name: "yaml becomes json",
			file: "x.yaml",
			in:   "cpu: 1\nmemory: 1Gi\ntags:\n  - a\n  - b\n",
			want: `{"cpu":1,"memory":"1Gi","tags":["a","b"]}`,
		},
		{
			name: "yaml comments and key order do not survive",
			file: "x.yml",
			in:   "# leading comment\nmemory: 1Gi   # trailing\ncpu: 1\n",
			want: `{"cpu":1,"memory":"1Gi"}`,
		},
		{
			name: "nested yaml",
			file: "x.yaml",
			in:   "limits:\n  cpu: 2\n  nested:\n    on: true\n",
			want: `{"limits":{"cpu":2,"nested":{"on":true}}}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonical(tc.file, []byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("Canonical() = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestCanonicalErrors(t *testing.T) {
	tests := []struct {
		name string
		file string
		in   string
	}{
		{"unsupported extension", "notes.txt", "hello"},
		{"invalid json", "x.json", `{"a":`},
		{"trailing content", "x.json", `{"a":1} {"b":2}`},
		{"non-string yaml key", "x.yaml", "1: one\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonical(tc.file, []byte(tc.in)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}
