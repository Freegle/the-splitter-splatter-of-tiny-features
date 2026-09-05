package evals

import (
	"reflect"
	"testing"
)

func TestCompanionSourcePaths(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "go test file",
			path: "/repo/internal/store/foo_test.go",
			want: []string{"/repo/internal/store/foo.go"},
		},
		{
			name: "nested go test file",
			path: "/repo/pkg/subdir/example_test.go",
			want: []string{"/repo/pkg/subdir/example.go"},
		},
		{
			name: "root level go test file",
			path: "/repo/main_test.go",
			want: []string{"/repo/main.go"},
		},
		{
			name: "js spec in tests/unit layout",
			path: "/repo/tests/unit/components/Button.spec.js",
			want: []string{
				"/repo/components/Button.vue",
				"/repo/components/Button.js",
				"/repo/components/Button.ts",
				"/repo/tests/unit/components/Button.vue",
				"/repo/tests/unit/components/Button.js",
				"/repo/tests/unit/components/Button.ts",
			},
		},
		{
			name: "ts spec in tests/unit layout",
			path: "/repo/tests/unit/utils/helper.spec.ts",
			want: []string{
				"/repo/utils/helper.vue",
				"/repo/utils/helper.js",
				"/repo/utils/helper.ts",
				"/repo/tests/unit/utils/helper.vue",
				"/repo/tests/unit/utils/helper.js",
				"/repo/tests/unit/utils/helper.ts",
			},
		},
		{
			name: "ts spec without tests/unit layout",
			path: "/app/components/Card.spec.ts",
			want: []string{
				"/app/components/Card.vue",
				"/app/components/Card.js",
				"/app/components/Card.ts",
			},
		},
		{
			name: "js spec without tests/unit layout",
			path: "/src/app/button.spec.js",
			want: []string{
				"/src/app/button.vue",
				"/src/app/button.js",
				"/src/app/button.ts",
			},
		},
		{
			name: "php Test in Unit layout",
			path: "/repo/tests/Unit/Services/UserTest.php",
			want: []string{
				"/repo/app/Services/User.php",
				"/repo/src/Services/User.php",
				"/repo/tests/Unit/Services/User.php",
			},
		},
		{
			name: "php Test in Feature layout",
			path: "/repo/tests/Feature/Auth/LoginTest.php",
			want: []string{
				"/repo/app/Auth/Login.php",
				"/repo/src/Auth/Login.php",
				"/repo/tests/Feature/Auth/Login.php",
			},
		},
		{
			name: "php test in tests/Feature with nested path",
			path: "/repo/tests/Feature/Users/CreateTest.php",
			want: []string{
				"/repo/app/Users/Create.php",
				"/repo/src/Users/Create.php",
				"/repo/tests/Feature/Users/Create.php",
			},
		},
		{
			name: "php test in tests/Unit with nested path",
			path: "/repo/tests/Unit/Services/Auth/LoginTest.php",
			want: []string{
				"/repo/app/Services/Auth/Login.php",
				"/repo/src/Services/Auth/Login.php",
				"/repo/tests/Unit/Services/Auth/Login.php",
			},
		},
		{
			name: "php test without tests/ prefix",
			path: "/app/UserTest.php",
			want: []string{"/app/User.php"},
		},
		{
			name: "non-matching file",
			path: "/repo/README.md",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := companionSourcePaths(tt.path)
			if len(got) == 0 && tt.want == nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("companionSourcePaths(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestCompanionSourcePaths_EdgeCases(t *testing.T) {
	tests := []struct {
		name string
		path string
		want []string
	}{
		{
			name: "empty path",
			path: "",
			want: nil,
		},
		{
			name: "path with no extension",
			path: "/repo/README",
			want: nil,
		},
		{
			name: "path ending with slash",
			path: "/repo/tests/unit/",
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := companionSourcePaths(tt.path)
			if len(got) == 0 && tt.want == nil {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("companionSourcePaths(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
