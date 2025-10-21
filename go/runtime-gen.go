package _go

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

type RuntimeServiceInfo struct {
	ServiceStructName string
}

type RuntimeInfo struct {
	Services []RuntimeServiceInfo
}

const runtimeFileTemplate = `package _polycode

import (
	runtime "github.com/cloudimpl/polycode-runtime-go"
)

func init() {
	var err error
	{{range .Services}}err = runtime.RegisterService(&{{.ServiceStructName}}{})
	if err != nil {
		panic(err)
	}
	{{end}}
}
`

func generateRuntime(appPath string) error {
	servicesFolder := filepath.Join(appPath, "services")

	if _, err := os.Stat(servicesFolder); os.IsNotExist(err) {
		println("No services folder found")
	} else {
		entries, err := os.ReadDir(servicesFolder)
		if err != nil {
			fmt.Printf("Error reading directory: %v\n", err)
			return err
		}

		var services []RuntimeServiceInfo
		for i, entry := range entries {
			fmt.Printf("Processing entry [%d/%d]\n", i+1, len(entries))
			if entry.IsDir() {
				serviceName := entry.Name()
				serviceStructName := toPascalCase(serviceName)
				services = append(services, RuntimeServiceInfo{
					ServiceStructName: serviceStructName,
				})
			}
		}

		// Use template to generate the code
		var buf bytes.Buffer
		tmpl, err := template.New("runtime-file-template").Parse(runtimeFileTemplate)
		if err != nil {
			return err
		}

		runtimeInfo := RuntimeInfo{
			Services: services,
		}

		err = tmpl.Execute(&buf, runtimeInfo)
		if err != nil {
			return err
		}

		err = os.WriteFile(appPath+"/.polycode/runtime.go", []byte(buf.String()), 0644)
		if err != nil {
			fmt.Printf("Error writing file: %v\n", err)
			return err
		}
	}

	return nil
}
