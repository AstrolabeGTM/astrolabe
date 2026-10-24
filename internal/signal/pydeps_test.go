package signal

import (
	"slices"
	"testing"
)

func TestPythonDeps(t *testing.T) {
	req := PythonRequirements([]byte("# c\nFastAPI[all]>=0.1\nsome_pkg==1.0 ; python_version>'3'\n-r other.txt\n\nrequests\n"))
	if !slices.Equal(req, []string{"fastapi", "some-pkg", "requests"}) {
		t.Fatalf("%v", req)
	}
	py := PyprojectDeps([]byte(`[project]
name = "x"
dependencies = [
  "Django>=4",
  'celery[redis]~=5.3',
]
[project.optional-dependencies]
dev = ["pytest"]

[tool.poetry.dependencies]
python = "^3.11"
Flask_Cors = "^4"
httpx = {version = "1"}
`))
	if !slices.Equal(py, []string{"django", "celery", "flask-cors", "httpx"}) {
		t.Fatalf("%v", py)
	}
	one := PyprojectDeps([]byte("[project]\ndependencies = [\"a_b\", \"c\"]\n"))
	if !slices.Equal(one, []string{"a-b", "c"}) {
		t.Fatalf("%v", one)
	}
}
