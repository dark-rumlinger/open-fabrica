// Package all registers every LLM-callable tool category. Each category package
// self-registers its tools in init(), so importing this package for side effects
// is the one way to load the full catalogue: the binary and every test that needs
// the real registry import it instead of repeating the category list.
//
// A new category directory under tools/ must be added here; TestAllCategoriesImported
// fails the build until it is.
package all

import (
	_ "openfabrica/tools/api"           // register api tools via init()
	_ "openfabrica/tools/auth"          // register auth tools via init()
	_ "openfabrica/tools/automation"    // register automation tools via init()
	_ "openfabrica/tools/content"       // register content tools via init()
	_ "openfabrica/tools/data"          // register data tools via init()
	_ "openfabrica/tools/messaging"     // register messaging tools via init()
	_ "openfabrica/tools/observability" // register observability tools via init()
	_ "openfabrica/tools/payments"      // register payments tools via init()
	_ "openfabrica/tools/project"       // register project tools via init()
)
