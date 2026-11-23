package evalkit

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/tree"
)

func TestPHPLocateNames(t *testing.T) {
	project := tree.NewMapTree(map[string]string{
		"tests/HttpTest.php":              "<?php\nnamespace Root\\Test;\nfinal class HttpTest extends CommonTest {}",
		"component/tests/UnusualFile.php": "<?php\nnamespace Component\\Test {\nclass HttpTest extends CommonTest {}\n}",
		"component/tests/OtherTest.php":   "<?php\nnamespace Component\\Test;\nclass OtherTest {}",
	})
	names := []string{`Root\Test\HttpTest::testResponse with data set "xml"`, "HttpTest::testResponse", "MissingTest::testResponse"}
	got, err := LocatePHPTests(project, names, map[string]bool{"tests/HttpTest.php": true})
	if err != nil {
		t.Fatal(err)
	}
	want := []PHPTestLoc{
		{Name: names[0], Files: []string{"tests/HttpTest.php"}, InPatch: true},
		{Name: names[1], Files: []string{"component/tests/UnusualFile.php", "tests/HttpTest.php"}, InPatch: true},
		{Name: names[2]},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("locations = %#v, want %#v", got, want)
	}
}

func TestPHPOracleProjectOwners(t *testing.T) {
	project := tree.NewMapTree(map[string]string{
		"composer.json":                   "{}",
		"component/composer.json":         "{}",
		"phpunit.xml.dist":                `<phpunit><testsuites><testsuite name="all"><directory suffix="Test.php">component/tests</directory><exclude>component/tests/ExcludedTest.php</exclude></testsuite></testsuites></phpunit>`,
		"dev/tests/unit/phpunit.xml.dist": `<phpunit><testsuites><testsuite name="aggregate"><directory>../../../comp*/test?</directory></testsuite></testsuites></phpunit>`,
		"component/phpunit.xml":           `<phpunit><testsuites><testsuite name="unit"><directory>tests</directory></testsuite></testsuites></phpunit>`,
	})
	files := []string{"component/tests/FooTest.php", "component/tests/ExcludedTest.php", "tests/RootTest.php"}
	got, err := phpTestOwners(project, files)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		files[0]: {".::php", "component::php", "dev/tests/unit::php"},
		files[1]: {"component::php", "dev/tests/unit::php"},
		files[2]: {".::php"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owners = %#v, want %#v", got, want)
	}
}
