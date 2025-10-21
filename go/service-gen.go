package _go

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"unicode"
)

type MethodInfo struct {
	OriginalName      string
	Name              string
	Description       string
	InputType         string
	IsInputPointer    bool
	IsInputPrimitive  bool
	OutputType        string
	IsOutputPointer   bool
	IsOutputPrimitive bool
	IsWorkflow        bool
	IsService         bool
}

type ServiceInfo struct {
	ModuleName        string
	ServiceName       string
	ServiceStructName string
	Methods           []MethodInfo
	IsProduction      bool // New flag to determine if we are in production mode
	Imports           []string
}

const serviceFileTemplate = `package _polycode

import (
	"errors"
	"github.com/cloudimpl/polycode-runtime-go/sdk"
	"strings"
    service "{{.ModuleName}}/services/{{.ServiceName}}"
	{{range .Imports}}"{{.}}"
	{{end}}
)

type {{.ServiceStructName}} struct {
}

func (t *{{.ServiceStructName}}) GetName() string {
	return "{{.ServiceName}}"
}

func (t *{{.ServiceStructName}}) GetDescription(method string) (string, error) {
	method = strings.ToLower(method)
	switch method {
	{{range .Methods}}case "{{.Name}}":
		{
			return "{{.Description}}", nil
		}
	{{end}}default:
		{
			return "", errors.New("method not found")
		}
	}
}

func (t *{{.ServiceStructName}}) GetInputType(method string) (any, error) {
	method = strings.ToLower(method)
	switch method {
	{{range .Methods}}case "{{.Name}}":
		{
			return &{{.InputType}}{}, nil
		}
	{{end}}default:
		{
			return nil, errors.New("method not found")
		}
	}
}

func (t *{{.ServiceStructName}}) GetOutputType(method string) (any, error) {
	switch strings.ToLower(method) {
	{{range .Methods}}
	case "{{.Name}}":
		{{if .IsOutputPrimitive}}
		var v {{.OutputType}}
		return &v, nil
		{{else}}
		return &{{.OutputType}}{}, nil
		{{end}}
	{{end}}
	default:
		return nil, fmt.Errorf("method %q not found", method)
	}
}

// ExecuteService handles methods with sdk.ServiceContext as the first parameter
func (t *{{.ServiceStructName}}) ExecuteService(ctx sdk.ServiceContext, method string, input any) (any, error) {
	method = strings.ToLower(method)

	{{if .IsProduction}}
	// Handle @definition case
	if method == "@definition" {
		return []string{
			{{range .Methods}}"{{.OriginalName}}",
			{{end}}
		}, nil
	}
	{{end}}

	switch method {
	{{range .Methods}}{{if .IsService}}case "{{.Name}}":
		{
			// Pass the input correctly as a pointer or value based on the method signature
			{{if .IsInputPointer}}
			return service.{{.OriginalName}}(ctx, input.(*{{.InputType}}))
			{{else}}
			return service.{{.OriginalName}}(ctx, *(input.(*{{.InputType}})))
			{{end}}
		}
		{{end}}{{end}}default:
		{
			return nil, errors.New("method not found")
		}
	}
}

// ExecuteWorkflow handles methods with sdk.WorkflowContext as the first parameter
func (t *{{.ServiceStructName}}) ExecuteWorkflow(ctx sdk.WorkflowContext, method string, input any) (any, error) {
	method = strings.ToLower(method)

	switch method {
	{{range .Methods}}{{if .IsWorkflow}}case "{{.Name}}":
		{
			// Pass the input correctly as a pointer or value based on the method signature
			{{if .IsInputPointer}}
			return service.{{.OriginalName}}(ctx, input.(*{{.InputType}}))
			{{else}}
			return service.{{.OriginalName}}(ctx, *(input.(*{{.InputType}})))
			{{end}}
		}
		{{end}}{{end}}default:
		{
			return nil, errors.New("method not found")
		}
	}
}

// IsWorkflow checks whether the method is a workflow (i.e., its first parameter is sdk.WorkflowContext)
func (t *{{.ServiceStructName}}) IsWorkflow(method string)bool {
	method = strings.ToLower(method)
	switch method {
	{{range .Methods}}{{if .IsWorkflow}}case "{{.Name}}":
		{
			return true
		}
		{{end}}{{end}}
	}
	return false
}
`

// extractDescriptionFromComments extracts the @description value from []*ast.Comment.
func extractDescriptionFromComments(comments []*ast.Comment) string {
	for _, c := range comments {
		line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		line = strings.TrimSpace(strings.TrimPrefix(line, "/*")) // handle block comment
		line = strings.TrimSpace(strings.TrimSuffix(line, "*/"))

		if strings.HasPrefix(line, "@description") {
			return strings.TrimSpace(strings.TrimPrefix(line, "@description"))
		}
	}
	return ""
}

// GetModuleName reads the go.mod file and extracts the module name
func getModuleName(filePath string) (string, error) {
	// Open go.mod file
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open go.mod file: %w", err)
	}
	defer file.Close()

	// Scan the file line by line
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		// Check if the line starts with "module"
		if strings.HasPrefix(line, "module") {
			// Split the line and get the module name
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1], nil // Return the module name
			}
		}
	}

	// Check for errors during scanning
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading go.mod file: %w", err)
	}

	return "", fmt.Errorf("module name not found in go.mod")
}

func generateService(appPath string, servicePath string, moduleName string, serviceName string, prod bool) error {
	methods, imports, err := parseDir(servicePath)
	if err != nil {
		fmt.Printf("Error parsing directory: %v\n", err)
		return err
	}

	if methods == nil {
		fmt.Printf("No methods found in the directory\n")
		return nil
	}

	generatedCode, err := generateServiceCode(moduleName, serviceName, methods, imports, prod)
	if err != nil {
		fmt.Printf("Error generating code: %v\n", err)
		return err
	}

	err = os.MkdirAll(appPath+"/.polycode", 0755)
	if err != nil {
		fmt.Printf("Error creating directory: %v\n", err)
		return err
	}

	err = os.WriteFile(appPath+"/.polycode/"+serviceName+".go", []byte(generatedCode), 0644)
	if err != nil {
		fmt.Printf("Error writing file: %v\n", err)
		return err
	}

	return nil
}

func generateServices(appPath string, prod bool) error {
	moduleName, err := getModuleName(appPath + "/go.mod")
	if err != nil {
		fmt.Printf("Error getting module name: %v\n", err)
		return err
	}

	polycodeFolder := filepath.Join(appPath, ".polycode")
	servicesFolder := filepath.Join(appPath, "services")

	if _, err = os.Stat(servicesFolder); os.IsNotExist(err) {
		println("No services folder found")
	} else {
		entries, err := os.ReadDir(servicesFolder)
		if err != nil {
			fmt.Printf("Error reading directory: %v\n", err)
			return err
		}

		for i, entry := range entries {
			fmt.Printf("Processing entry [%d/%d]\n", i+1, len(entries))
			if entry.IsDir() {
				servicePath := filepath.Join(servicesFolder, entry.Name())
				println("Generating code for path: ", servicePath)
				serviceName := entry.Name()
				err = generateService(appPath, servicePath, moduleName, serviceName, prod)
				if err != nil {
					fmt.Printf("Error generating service: %v\n", err)
					return err
				}
				println("Generated code for path: ", servicePath)
			}
		}

		println("Finished generating code for services")
	}

	if _, err = os.Stat(polycodeFolder); !os.IsNotExist(err) {
		println("Cleaning up imports")
		err = runGoImports(polycodeFolder)
		if err != nil {
			fmt.Printf("Error cleaning up imports: %v\n", err)
			return err
		}
		println("Imports cleaned")
	}

	return nil
}

// Modified validateFunctionParams to check for sdk.ServiceContext or sdk.WorkflowContext
func validateFunctionParams(fn *ast.FuncDecl) (string, error) {
	// Check if there are at least two parameters (ctx and input)
	if fn.Type.Params == nil || len(fn.Type.Params.List) < 2 {
		return "", fmt.Errorf("function %s does not have enough parameters", fn.Name.Name)
	}

	// Validate the first parameter type
	firstParam := fn.Type.Params.List[0].Type
	if starExpr, ok := firstParam.(*ast.SelectorExpr); ok {
		if starExpr.X.(*ast.Ident).Name == "sdk" {
			// Check if the first parameter is either ServiceContext or WorkflowContext
			if starExpr.Sel.Name == "ServiceContext" {
				return "Service", nil
			} else if starExpr.Sel.Name == "WorkflowContext" {
				return "Workflow", nil
			} else {
				return "", fmt.Errorf("function %s: first parameter must be sdk.ServiceContext or sdk.WorkflowContext", fn.Name.Name)
			}
		}
	}
	return "", fmt.Errorf("function %s: first parameter must be sdk.ServiceContext or sdk.WorkflowContext", fn.Name.Name)
}

func extractType(expr ast.Expr) (typeStr string, isPointer bool, isPrimitive bool) {
	switch t := expr.(type) {

	case *ast.StarExpr:
		innerType, _, primitive := extractType(t.X)
		return innerType, true, primitive

	case *ast.SelectorExpr:
		// Handles pkg.Type
		if pkgIdent, ok := t.X.(*ast.Ident); ok {
			typeName := fmt.Sprintf("%s.%s", pkgIdent.Name, t.Sel.Name)
			return typeName, false, false
		}

		return t.Sel.Name, false, false

	case *ast.Ident:
		// Handles builtin and local types
		return t.Name, false, primitiveTypes[t.Name]

	case *ast.ArrayType:
		elemType, _, _ := extractType(t.Elt)
		return "[]" + elemType, false, false

	case *ast.MapType:
		keyType, _, _ := extractType(t.Key)
		valType, _, _ := extractType(t.Value)
		return fmt.Sprintf("map[%s]%s", keyType, valType), false, false

	case *ast.InterfaceType:
		return "interface{}", false, false

	default:
		return fmt.Sprintf("%T", t), false, false
	}
}

var primitiveTypes = map[string]bool{
	"string": true, "bool": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "uint": true, "uint8": true, "uint16": true,
	"uint32": true, "uint64": true, "float32": true, "float64": true,
	"byte": true, "rune": true, "any": true, "interface{}": true,
}

// Updated parseDir function to mark methods as workflow or service
func parseDir(serviceFolder string) ([]MethodInfo, []string, error) {
	fset := token.NewFileSet()

	var methods []MethodInfo
	var imports []string

	err := filepath.Walk(serviceFolder, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Only process Go files that are not test files
		if strings.HasSuffix(info.Name(), ".go") && !strings.HasSuffix(info.Name(), "_test.go") {
			node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return err
			}

			// Collect all imports from this file
			for _, imp := range node.Imports {
				importPath := strings.Trim(imp.Path.Value, "\"")
				imports = append(imports, importPath)
			}

			for _, decl := range node.Decls {
				if fn, isFn := decl.(*ast.FuncDecl); isFn && fn.Recv == nil {
					OriginalName := fn.Name.Name

					// check if function name starts with simple letter
					if unicode.IsLower(rune(OriginalName[0])) {
						continue
					}

					// Validate the function's parameters
					contextType, err := validateFunctionParams(fn)
					if err != nil {
						return err
					}

					// Extract the function name and input/output parameters
					methodName := strings.ToLower(fn.Name.Name) // Normalize to lowercase
					var description string

					if fn.Doc == nil || len(fn.Doc.List) == 0 {
						description = ""
					} else {
						description = extractDescriptionFromComments(fn.Doc.List)
					}
					inputType, isInputPointer, isInputPrimitive := extractType(fn.Type.Params.List[1].Type)
					outputType, isOutputPointer, isOutputPrimitive := extractType(fn.Type.Results.List[0].Type)

					// Append the method and its corresponding input type to methods
					if inputType != "" && outputType != "" {
						methods = append(methods, MethodInfo{
							OriginalName:      OriginalName,
							Name:              methodName,
							Description:       description,
							InputType:         inputType,
							IsInputPointer:    isInputPointer,
							IsInputPrimitive:  isInputPrimitive,
							OutputType:        outputType,
							IsOutputPointer:   isOutputPointer,
							IsOutputPrimitive: isOutputPrimitive,
							IsWorkflow:        contextType == "Workflow",
							IsService:         contextType == "Service",
						})
					}
				}
			}
		}
		return nil
	})

	if err != nil {
		return nil, nil, err
	}

	// Remove duplicate imports
	imports = unique(imports)
	return methods, imports, nil
}

// Helper function to remove duplicate import paths
func unique(strings []string) []string {
	uniqueStrings := make(map[string]bool)
	var result []string
	for _, str := range strings {
		if _, exists := uniqueStrings[str]; !exists {
			uniqueStrings[str] = true
			result = append(result, str)
		}
	}
	return result
}

func toPascalCase(input string) string {
	// Split the string by hyphens
	words := strings.Split(input, "-")

	// Capitalize the first letter of each word
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(string(word[0])) + word[1:]
		}
	}

	// Join words to form PascalCase
	return strings.Join(words, "")
}

// GenerateService the wrapper code based on the extracted information
func generateServiceCode(moduleName string, serviceName string, methods []MethodInfo, imports []string, isProd bool) (string, error) {
	serviceStructName := toPascalCase(serviceName)

	serviceInfo := ServiceInfo{
		ModuleName:        moduleName,
		ServiceName:       serviceName,
		ServiceStructName: serviceStructName,
		Methods:           methods,
		IsProduction:      isProd,
		Imports:           imports,
	}

	// Use template to generate the code
	var buf bytes.Buffer
	tmpl, err := template.New("service-file-template").Parse(serviceFileTemplate)
	if err != nil {
		return "", err
	}

	err = tmpl.Execute(&buf, serviceInfo)
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}

// RunGoImports runs goimports on the generated file to remove unnecessary imports
func runGoImports(filePath string) error {
	cmd := exec.Command("goimports", "-w", filePath)
	return cmd.Run()
}

func CheckFileCompilable(fileName string) error {
	// Execute the `go build` command for the file
	cmd := exec.Command("go", "build", "-o", "/dev/null", fileName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("compilation error: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func IsGoFile(fileName string) bool {
	// Ensure the file ends with .go
	return strings.HasSuffix(fileName, ".go")
}
