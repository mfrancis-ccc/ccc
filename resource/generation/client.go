// package generation provides the ability to generate resource, handler, and typescript permissions and metadata code from a resource file.
package generation

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	cloudspanner "cloud.google.com/go/spanner"
	"github.com/cccteam/ccc/accesstypes"
	"github.com/cccteam/ccc/resource"
	initiator "github.com/cccteam/db-initiator"
	"github.com/cccteam/spxscan"
	"github.com/ettle/strcase"
	"github.com/go-playground/errors/v5"
	"github.com/momaek/formattag/align"
	"golang.org/x/tools/imports"
)

type Client struct {
	genHandlers           func() error
	genTypescriptPerm     func() error
	genTypescriptMeta     func() error
	genRoutes             func() error
	resourceFilePath      string
	resources             []*ResourceInfo
	resourceTree          *ast.File
	resourceDestination   string
	handlerDestination    string
	typescriptDestination string
	routerDestination     string
	routerPackage         string
	routePrefix           string
	rc                    *resource.Collection
	db                    *cloudspanner.Client
	caser                 *strcase.Caser
	tableLookup           map[string]*TableMetadata
	handlerOptions        map[string]map[HandlerType][]OptionType
	pluralOverrides       map[string]string
	typescriptOverrides   map[string]string
	cleanup               func()

	muAlign sync.Mutex
}

func New(ctx context.Context, resourcePackageDir, migrationSourceURL string, generatorOptions ...ClientOption) (*Client, error) {
	spannerContainer, err := initiator.NewSpannerContainer(ctx, "latest")
	if err != nil {
		return nil, errors.Wrap(err, "initiator.NewSpannerContainer()")
	}

	db, err := spannerContainer.CreateDatabase(ctx, "resourcegeneration")
	if err != nil {
		return nil, errors.Wrap(err, "container.CreateDatabase()")
	}

	cleanupFunc := func() {
		if err := db.DropDatabase(ctx); err != nil {
			panic(err)
		}

		if err := db.Close(); err != nil {
			panic(err)
		}
	}

	if err := db.MigrateUp(migrationSourceURL); err != nil {
		return nil, errors.Wrap(err, "db.MigrateUp()")
	}

	c := &Client{
		db:                  db.Client,
		resourceDestination: resourcePackageDir,
		cleanup:             cleanupFunc,
		caser:               strcase.NewCaser(false, nil, nil),
	}

	for _, optionFunc := range generatorOptions {
		if err := optionFunc(c); err != nil {
			return nil, err
		}
	}

	c.tableLookup, err = c.createTableLookup(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "c.createTableLookup()")
	}

	err = removeGeneratedFiles(resourcePackageDir, Prefix)
	if err != nil {
		return nil, errors.Wrap(err, "removeGeneratedFilesInNewClient()")
	}

	pkg, err := loadPackage(resourcePackageDir)
	if err != nil {
		return nil, err
	}

	c.resourceFilePath = filepath.Join(resourcePackageDir, filepath.Base(pkg.GoFiles[0]))

	c.resources, err = c.extractResourceTypes(pkg.Types)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (c *Client) Close() {
	c.cleanup()
}

func (c *Client) RunGeneration() error {
	if err := c.runResourcesGeneration(); err != nil {
		return errors.Wrap(err, "c.genResources()")
	}
	if c.genHandlers != nil {
		if err := c.genHandlers(); err != nil {
			return errors.Wrap(err, "c.genHandlers()")
		}
	}
	if c.genRoutes != nil {
		if err := c.genRoutes(); err != nil {
			return errors.Wrap(err, "c.genRoutes()")
		}
	}
	if c.genTypescriptMeta != nil {
		if err := c.genTypescriptMeta(); err != nil {
			return errors.Wrap(err, "c.genTypescriptMeta()")
		}
	}
	if c.genTypescriptPerm != nil {
		if err := c.genTypescriptPerm(); err != nil {
			return errors.Wrap(err, "c.genTypescriptPerm()")
		}
	}

	return nil
}

func (c *Client) createTableLookup(ctx context.Context) (map[string]*TableMetadata, error) {
	qry := `WITH DEPENDENCIES AS (
		SELECT DISTINCT
			kcu1.TABLE_NAME, 
			kcu1.COLUMN_NAME, 
			(SUM(CASE tc.CONSTRAINT_TYPE WHEN 'PRIMARY KEY' THEN 1 ELSE 0 END)) AS IS_PRIMARY_KEY,
			(SUM(CASE tc.CONSTRAINT_TYPE WHEN 'FOREIGN KEY' THEN 1 ELSE 0 END)) AS IS_FOREIGN_KEY,
			kcu1.ORDINAL_POSITION AS KEY_ORDINAL_POSITION,
			(CASE MIN(CASE 
					WHEN kcu4.TABLE_NAME IS NOT NULL THEN 1
					WHEN kcu2.TABLE_NAME IS NOT NULL THEN 2
					ELSE 3
					END)
			WHEN 1 THEN MAX(kcu4.TABLE_NAME)
			WHEN 2 THEN MAX(kcu2.TABLE_NAME)
			ELSE NULL
			END) AS REFERENCED_TABLE,
			(CASE MIN(CASE 
					WHEN kcu4.COLUMN_NAME IS NOT NULL THEN 1
					WHEN kcu2.COLUMN_NAME IS NOT NULL THEN 2
					ELSE 3
					END)
			WHEN 1 THEN MAX(kcu4.COLUMN_NAME)
			WHEN 2 THEN MAX(kcu2.COLUMN_NAME)
			ELSE NULL
			END) AS REFERENCED_COLUMN
		FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu1 -- All columns that are Primary Key or Foreign Key
		JOIN INFORMATION_SCHEMA.TABLE_CONSTRAINTS tc ON tc.CONSTRAINT_NAME = kcu1.CONSTRAINT_NAME -- Identify whether column is Primary Key or Foreign Key
		LEFT JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc ON rc.CONSTRAINT_NAME = kcu1.CONSTRAINT_NAME -- All unique constraints (e.g. PK_Persons) referenced by foreign key constraints (e.g. FK_PersonPhones_PersonId)
		LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu2 ON kcu2.CONSTRAINT_NAME = rc.UNIQUE_CONSTRAINT_NAME -- Table & Column belonging to referenced unique constraint (e.g. Persons, Id)
			AND kcu2.ORDINAL_POSITION = kcu1.ORDINAL_POSITION
		LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu3 ON kcu3.TABLE_NAME = kcu2.TABLE_NAME AND kcu3.COLUMN_NAME = kcu2.COLUMN_NAME
		LEFT JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS rc2 ON rc2.CONSTRAINT_NAME = kcu3.CONSTRAINT_NAME
		LEFT JOIN INFORMATION_SCHEMA.KEY_COLUMN_USAGE kcu4 ON kcu4.CONSTRAINT_NAME = rc2.UNIQUE_CONSTRAINT_NAME -- Table & Column belonging to 1-jump referenced unique constraint (e.g. DoeInstitutions, Id)
			AND kcu4.ORDINAL_POSITION = kcu1.ORDINAL_POSITION
		WHERE
			kcu1.CONSTRAINT_SCHEMA != 'INFORMATION_SCHEMA'
			AND tc.CONSTRAINT_TYPE IN ('PRIMARY KEY', 'FOREIGN KEY')
		GROUP BY kcu1.TABLE_NAME, kcu1.COLUMN_NAME, KEY_ORDINAL_POSITION
	)
	SELECT DISTINCT
		c.TABLE_NAME,
		c.COLUMN_NAME,
		(c.IS_NULLABLE = 'YES') AS IS_NULLABLE,
		c.SPANNER_TYPE,
		(d.IS_PRIMARY_KEY > 0 and d.IS_PRIMARY_KEY IS NOT NULL) as IS_PRIMARY_KEY,
		(d.IS_FOREIGN_KEY > 0 and d.IS_FOREIGN_KEY IS NOT NULL) as IS_FOREIGN_KEY,
		d.REFERENCED_TABLE,
		d.REFERENCED_COLUMN,
		(t.TABLE_NAME IS NULL AND v.TABLE_NAME IS NOT NULL) as IS_VIEW,
		ic.INDEX_NAME IS NOT NULL AS IS_INDEX,
		MAX(COALESCE(i.IS_UNIQUE, false)) AS IS_UNIQUE_INDEX,
		c.GENERATION_EXPRESSION,
		c.ORDINAL_POSITION,
		COALESCE(d.KEY_ORDINAL_POSITION, 1) AS KEY_ORDINAL_POSITION,
	FROM INFORMATION_SCHEMA.COLUMNS c
		LEFT JOIN INFORMATION_SCHEMA.TABLES t ON c.TABLE_NAME = t.TABLE_NAME
			AND t.TABLE_TYPE = 'BASE TABLE'
		LEFT JOIN INFORMATION_SCHEMA.VIEWS v ON c.TABLE_NAME = v.TABLE_NAME
		LEFT JOIN DEPENDENCIES d ON c.TABLE_NAME = d.TABLE_NAME
			AND c.COLUMN_NAME = d.COLUMN_NAME
		LEFT JOIN INFORMATION_SCHEMA.INDEX_COLUMNS ic ON c.COLUMN_NAME = ic.COLUMN_NAME
			AND c.TABLE_NAME = ic.TABLE_NAME
		LEFT JOIN INFORMATION_SCHEMA.INDEXES i ON ic.INDEX_NAME = i.INDEX_NAME 
	WHERE 
		c.TABLE_SCHEMA != 'INFORMATION_SCHEMA'
		AND c.COLUMN_NAME NOT LIKE '%_HIDDEN'
	GROUP BY c.TABLE_NAME, c.COLUMN_NAME, IS_NULLABLE, c.SPANNER_TYPE, d.IS_PRIMARY_KEY, d.IS_FOREIGN_KEY, d.REFERENCED_COLUMN, d.REFERENCED_TABLE, IS_VIEW, IS_INDEX, c.GENERATION_EXPRESSION, c.ORDINAL_POSITION, d.KEY_ORDINAL_POSITION
	ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION`

	return c.createLookupMapForQuery(ctx, qry)
}

func (c *Client) createLookupMapForQuery(ctx context.Context, qry string) (map[string]*TableMetadata, error) {
	log.Println("Creating spanner table lookup...")

	stmt := cloudspanner.Statement{SQL: qry}

	var result []InformationSchemaResult
	if err := spxscan.Select(ctx, c.db.Single(), &result, stmt); err != nil {
		return nil, errors.Wrap(err, "spxscan.Select()")
	}

	m := make(map[string]*TableMetadata)
	for _, r := range result {
		table, ok := m[r.TableName]
		if !ok {
			table = &TableMetadata{
				Columns:       make(map[string]ColumnMeta),
				SearchIndexes: make(map[string][]*expressionField),
				IsView:        r.IsView,
			}
		}

		if r.SpannerType == "TOKENLIST" {
			continue
		}

		column, ok := table.Columns[r.ColumnName]
		if !ok {
			column = ColumnMeta{
				ColumnName:         r.ColumnName,
				SpannerType:        r.SpannerType,
				OrdinalPosition:    r.OrdinalPosition - 1, // SQL is 1-indexed. For consistency with JavaScript & Go we translate to 0-indexed
				KeyOrdinalPosition: r.KeyOrdinalPosition - 1,
			}
		}

		if r.IsPrimaryKey {
			table.PkCount++
			column.IsPrimaryKey = true
			if !slices.Contains(column.ConstraintTypes, PrimaryKey) {
				column.ConstraintTypes = append(column.ConstraintTypes, PrimaryKey)
			}
		}

		if r.IsForeignKey {
			column.IsForeignKey = true
			if !slices.Contains(column.ConstraintTypes, ForeignKey) {
				column.ConstraintTypes = append(column.ConstraintTypes, ForeignKey)
			}

			if r.ReferencedTable != nil {
				column.ReferencedTable = *r.ReferencedTable
			}

			if r.ReferencedColumn != nil {
				column.ReferencedColumn = *r.ReferencedColumn
			}
		}

		if r.IsNullable {
			column.IsNullable = true
		}

		if r.IsIndex {
			column.IsIndex = true
		}

		if r.IsUniqueIndex {
			column.IsUniqueIndex = true
		}

		table.Columns[r.ColumnName] = column
		m[r.TableName] = table
	}

	for _, r := range result {
		if r.SpannerType == "TOKENLIST" {
			table := m[r.TableName]

			if r.GenerationExpression == nil {
				return nil, errors.Newf("generation expression not found for tokenlist column=`%s` table=`%s`", r.ColumnName, r.TableName)
			}

			expressionFields, err := searchExpressionFields(*r.GenerationExpression, table.Columns)
			if err != nil {
				return nil, errors.Wrapf(err, "searchExpressionFields table=`%s`", r.TableName)
			}

			table.SearchIndexes[r.ColumnName] = append(table.SearchIndexes[r.ColumnName], expressionFields...)
		}
	}

	return m, nil
}

func (c *Client) writeBytesToFile(destination string, file *os.File, data []byte, goFormat bool) error {
	if goFormat {
		var err error
		data, err = format.Source(data)
		if err != nil {
			return errors.Wrapf(err, "format.Source(): file: %s", file.Name())
		}

		data, err = imports.Process(destination, data, nil)
		if err != nil {
			return errors.Wrapf(err, "imports.Process(): file: %s", file.Name())
		}

		// align package is not concurrent safe
		c.muAlign.Lock()
		defer c.muAlign.Unlock()

		align.Init(bytes.NewReader(data))
		data, err = align.Do()
		if err != nil {
			return errors.Wrapf(err, "align.Do(): file: %s", file.Name())
		}
	}

	if err := file.Truncate(0); err != nil {
		return errors.Wrapf(err, "file.Truncate(): file: %s", file.Name())
	}
	if _, err := file.Seek(0, 0); err != nil {
		return errors.Wrapf(err, "file.Seek(): file: %s", file.Name())
	}
	if _, err := file.Write(data); err != nil {
		return errors.Wrapf(err, "file.Write(): file: %s", file.Name())
	}

	return nil
}

func (c *Client) templateFuncs() map[string]any {
	templateFuncs := map[string]any{
		"Pluralize": c.pluralize,
		"GoCamel":   strcase.ToGoCamel,
		"Camel":     c.caser.ToCamel,
		"Pascal":    c.caser.ToPascal,
		"Kebab":     c.caser.ToKebab,
		"Lower":     strings.ToLower,
		"PrimaryKeyTypeIsUUID": func(fields []*FieldInfo) bool {
			for _, f := range fields {
				if f.IsPrimaryKey {
					return f.GoType == "ccc.UUID"
				}
			}

			return false
		},
		"FormatPerm": func(s string) string {
			if s == "" {
				return ""
			}

			return ` perm:"` + s + `"`
		},
		"PrimaryKeyType": func(fields []*FieldInfo) string {
			for _, f := range fields {
				if f.IsPrimaryKey {
					return f.GoType
				}
			}

			return ""
		},
		"FormatQueryTag": func(query string) string {
			if query != "" {
				return " " + query
			}

			return ""
		},
		"FormatResourceInterfaceTypes": formatResourceInterfaceTypes,
		"FormatTokenTag":               c.formatTokenTags,
		"ResourceSearchType": func(searchType string) string {
			switch strings.ToUpper(searchType) {
			case "SUBSTRING":
				return "resource.SubString"
			case "FULLTEXT":
				return "resource.FullText"
			case "NGRAMS":
				return "resource.Ngram"
			default:
				return ""
			}
		},
		"DetermineTestURL": func(structName string, route generatedRoute) string {
			if strings.EqualFold(route.Method, "get") && strings.HasSuffix(route.Path, fmt.Sprintf("{%sID}", strcase.ToGoCamel(structName))) {
				return fmt.Sprintf("/%s/%s/%s",
					c.routePrefix,
					c.caser.ToKebab(c.pluralize(structName)),
					strcase.ToGoCamel(fmt.Sprintf("test%sID", c.caser.ToPascal(structName))),
				)
			}

			return route.Path
		},
		"DetermineParameters": func(structName string, route generatedRoute) string {
			if strings.EqualFold(route.Method, "get") && strings.HasSuffix(route.Path, fmt.Sprintf("{%sID}", strcase.ToGoCamel(structName))) {
				return fmt.Sprintf(`map[string]string{%q: %q}`, strcase.ToGoCamel(structName+"ID"), strcase.ToGoCamel(fmt.Sprintf("test%sID", c.caser.ToPascal(structName))))
			}

			return "map[string]string{}"
		},
		"MethodToHttpConst": func(method string) string {
			switch method {
			case "GET":
				return "http.MethodGet"
			case "POST":
				return "http.MethodPost"
			case "PATCH":
				return "http.MethodPatch"
			default:
				panic(fmt.Sprintf("MethodToHttpConst: unknown method: %s", method))
			}
		},
	}

	return templateFuncs
}

func (c *Client) pluralize(value string) string {
	if plural, ok := c.pluralOverrides[value]; ok {
		return plural
	}

	toLower := strings.ToLower(value)
	switch {
	case strings.HasSuffix(toLower, "y"):
		return value[:len(value)-1] + "ies"
	case strings.HasSuffix(toLower, "s"):
		return value + "es"
	default:
		return value + "s"
	}
}

func (c *Client) formatTokenTags(tableName, fieldName string) string {
	tokenIndexMap := make(map[string][]string)
	t, ok := c.tableLookup[tableName]
	if !ok {
		panic(fmt.Sprintf("table not found: %s", tableName))
	}

	for k, v := range t.SearchIndexes {
		for _, f := range v {
			if f.fieldName == fieldName {
				token := string(f.tokenType)
				tokenIndexMap[token] = append(tokenIndexMap[token], k)
			}
		}
	}

	var tags []string
	for tt, indexes := range tokenIndexMap {
		tags = append(tags, fmt.Sprintf(`%s:%q`, tt, strings.Join(indexes, ",")))
	}

	return strings.Join(tags, " ")
}

func removeGeneratedFiles(directory string, method GeneratedFileDeleteMethod) error {
	dir, err := os.Open(directory)
	if err != nil {
		return errors.Wrap(err, "os.Open()")
	}
	defer dir.Close()

	files, err := dir.Readdirnames(0)
	if err != nil {
		return errors.Wrap(err, "dir.Readdirnames()")
	}

	if err := dir.Close(); err != nil {
		return errors.Wrap(err, "dir.Close()")
	}

	for _, f := range files {
		if !strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, ".ts") {
			continue
		}

		switch method {
		case Prefix:
			if err := removeGeneratedFileByPrefix(directory, f); err != nil {
				return errors.Wrap(err, "removeGeneratedFileByPrefix()")
			}
		case HeaderComment:
			if err := removeGeneratedFileByHeaderComment(directory, f); err != nil {
				return errors.Wrap(err, "removeGeneratedFileByHeaderComment()")
			}
		}
	}

	return nil
}

func removeGeneratedFileByPrefix(directory, file string) error {
	if strings.HasPrefix(file, genPrefix) {
		fp := filepath.Join(directory, file)
		if err := os.Remove(fp); err != nil {
			return errors.Wrap(err, "os.Remove()")
		}
	}

	return nil
}

func removeGeneratedFileByHeaderComment(directory, file string) error {
	fp := filepath.Join(directory, file)
	content, err := os.ReadFile(fp)
	if err != nil {
		return errors.Wrap(err, "os.ReadFile()")
	}

	generationHeader := "// Code generated by resourcegeneration. DO NOT EDIT."
	if bytes.HasPrefix(content, []byte(generationHeader)) {
		if err := os.Remove(fp); err != nil {
			return errors.Wrap(err, "os.Remove()")
		}
	}

	return nil
}

func formatResourceInterfaceTypes(resources []*ResourceInfo) string {
	var resourceNames [][]string
	var resourceNamesLen int
	for i, t := range resources {
		resourceNamesLen += len(t.Name)
		if i == 0 || resourceNamesLen > 80 {
			resourceNamesLen = len(t.Name)
			resourceNames = append(resourceNames, []string{})
		}

		resourceNames[len(resourceNames)-1] = append(resourceNames[len(resourceNames)-1], t.Name)
	}

	var sb strings.Builder
	for _, row := range resourceNames {
		sb.WriteString("\n\t")
		for _, cell := range row {
			line := fmt.Sprintf("%s | ", cell)
			sb.WriteString(line)
		}
	}

	return strings.TrimSuffix(strings.TrimPrefix(sb.String(), "\n"), " | ")
}

func searchExpressionFields(expression string, cols map[string]ColumnMeta) ([]*expressionField, error) {
	var flds []*expressionField

	for _, match := range tokenizeRegex.FindAllStringSubmatch(expression, -1) {
		if len(match) != 3 {
			return nil, errors.Newf("expression `%s` has unexpected number of matches: `%d` (expected 3)", expression, len(match))
		}

		var tokenType resource.SearchType
		switch match[1] {
		case "TOKENIZE_SUBSTRING":
			tokenType = resource.SubString
		case "TOKENIZE_FULLTEXT":
			tokenType = resource.FullText
		case "TOKENIZE_NGRAMS":
			tokenType = resource.Ngram
		default:
			continue
		}

		fieldName := match[2]
		if _, ok := cols[fieldName]; !ok { // sanity check that the field name is a real column. just in case the regex leaks something improper
			return nil, errors.Newf("column `%s` from expression `%s` was not found in table (is the tokenizeRegex working?)", fieldName, match[0])
		}

		flds = append(flds, &expressionField{
			tokenType: tokenType,
			fieldName: match[2],
		})
	}

	return flds, nil
}

func (c *Client) isResourceRegisteredInRouter(resourceName string, routerResources []accesstypes.Resource) bool {
	return slices.Contains(routerResources, accesstypes.Resource(c.pluralize(resourceName)))
}
