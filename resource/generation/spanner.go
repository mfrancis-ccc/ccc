package generation

import (
	"bytes"
	"go/ast"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"text/template"

	"github.com/go-playground/errors/v5"
)

func (c *Client) runResourcesGeneration() error {
	if err := removeGeneratedFiles(c.resourceDestination, HeaderComment); err != nil {
		return errors.Wrap(err, "removeGeneratedFiles()")
	}

	types, err := c.buildPatcherTypesFromSource()
	if err != nil {
		return errors.Wrap(err, "c.buildPatcherTypesFromSource()")
	}

	if err := c.generateResourceInterfaces(types); err != nil {
		return errors.Wrap(err, "c.generateResourceInterfaces()")
	}

	for _, t := range types {
		if err := c.generatePatcherTypes(t); err != nil {
			return errors.Wrap(err, "c.generatePatcherTypes()")
		}
	}

	if err := c.generateResourceTests(types); err != nil {
		return errors.Wrap(err, "c.generateResourceTests()")
	}

	return nil
}

func (c *Client) generateResourceInterfaces(types []*generatedType) error {
	output, err := c.generateTemplateOutput(resourcesInterfaceTemplate, map[string]any{
		"Source": c.resourceFilePath,
		"Types":  types,
	})
	if err != nil {
		return errors.Wrap(err, "generateTemplateOutput()")
	}

	destinationFile := filepath.Join(c.resourceDestination, generatedFileName(resourceInterfaceOutputName))

	file, err := os.Create(destinationFile)
	if err != nil {
		return errors.Wrap(err, "os.Create()")
	}
	defer file.Close()

	if err := c.writeBytesToFile(destinationFile, file, output, true); err != nil {
		return errors.Wrap(err, "c.writeBytesToFile()")
	}

	return nil
}

func (c *Client) generateResourceTests(types []*generatedType) error {
	output, err := c.generateTemplateOutput(resourcesTestTemplate, map[string]any{
		"Source": c.resourceFilePath,
		"Types":  types,
	})
	if err != nil {
		return errors.Wrap(err, "generateTemplateOutput()")
	}

	destinationFile := filepath.Join(c.resourceDestination, generatedFileName(resourcesTestName))

	file, err := os.Create(destinationFile)
	if err != nil {
		return errors.Wrap(err, "os.Create()")
	}
	defer file.Close()

	if err := c.writeBytesToFile(destinationFile, file, output, true); err != nil {
		return errors.Wrap(err, "c.writeBytesToFile()")
	}

	return nil
}

func (c *Client) generatePatcherTypes(generatedType *generatedType) error {
	fileName := generatedFileName(strings.ToLower(c.caser.ToSnake(c.pluralize(generatedType.Name))))
	destinationFilePath := filepath.Join(c.resourceDestination, fileName)

	log.Printf("Generating spanner file: %v\n", fileName)

	output, err := c.generateTemplateOutput(resourceFileTemplate, map[string]any{
		"Source":          c.resourceFilePath,
		"Name":            generatedType.Name,
		"IsView":          generatedType.IsView,
		"Fields":          generatedType.Fields,
		"IsCompoundTable": generatedType.IsCompoundTable,
	})
	if err != nil {
		return errors.Wrap(err, "generateTemplateOutput()")
	}

	file, err := os.Create(destinationFilePath)
	if err != nil {
		return errors.Wrap(err, "os.Create()")
	}
	defer file.Close()

	if err := c.writeBytesToFile(destinationFilePath, file, output, true); err != nil {
		return errors.Wrap(err, "c.writeBytesToFile()")
	}

	return nil
}

func (c *Client) buildPatcherTypesFromSource() ([]*generatedType, error) {
	typeList := make([]*generatedType, 0)
	for _, d := range c.resourceTree.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok {
			continue
		}

		for _, s := range gd.Specs {
			ts, ok := s.(*ast.TypeSpec)
			if !ok {
				continue
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			if st.Fields == nil {
				continue
			}

			isCompoundTable := true
			tableName := c.pluralize(ts.Name.Name)

			table, ok := c.tableLookup[tableName]
			if !ok || table == nil {
				return nil, errors.Newf("table not found: %s", tableName)
			}

			fields := make([]*typeField, 0)
			for _, f := range st.Fields.List {
				if len(f.Names) == 0 {
					continue
				}

				fields = append(fields, c.typeFieldFromAstField(table, f, &isCompoundTable))
			}

			var isView bool
			if table, ok := c.tableLookup[tableName]; ok {
				isView = table.IsView
			}

			typeList = append(typeList, &generatedType{
				Name:            ts.Name.Name,
				Fields:          fields,
				IsCompoundTable: isCompoundTable == (len(fields) > 1),
				IsView:          isView,
			})
		}
	}

	sort.Slice(typeList, func(i, j int) bool {
		return typeList[i].Name < typeList[j].Name
	})

	return typeList, nil
}

func (c *Client) typeFieldFromAstField(tableMetadata *TableMetadata, f *ast.Field, isCompoundTable *bool) *typeField {
	field := &typeField{
		Name: f.Names[0].Name,
	}

	field.Type = fieldType(f.Type, false)

	if f.Tag != nil {
		field.Tag = f.Tag.Value
	}

	if field.Tag == "" {
		return field
	}

	structTag := reflect.StructTag(field.Tag[1 : len(field.Tag)-1])
	column := structTag.Get("spanner")

	if data, ok := tableMetadata.Columns[column]; ok {
		field.IsPrimaryKey = data.ConstraintType == PrimaryKey
		field.IsIndex = data.IsIndex
		field.IsUniqueIndex = data.IsUniqueIndex

		if data.ConstraintType != PrimaryKey && data.ConstraintType != ForeignKey {
			*isCompoundTable = false
		}
	}

	return field
}

func (c *Client) generateTemplateOutput(fileTemplate string, data map[string]any) ([]byte, error) {
	tmpl, err := template.New(fileTemplate).Funcs(c.templateFuncs()).Parse(fileTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "template.Parse()")
	}

	buf := bytes.NewBuffer([]byte{})
	if err := tmpl.Execute(buf, data); err != nil {
		return nil, errors.Wrap(err, "tmpl.Execute()")
	}

	return buf.Bytes(), nil
}
