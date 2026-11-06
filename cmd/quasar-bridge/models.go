package main

import (
	"context"
	"fmt"
	"io"

	"quasarbridge/internal/llm"
)

// Model IDs are validated by the client; labels are quoted so model metadata
// cannot inject terminal controls or extra records into the installer's menu.
func writeLLMModels(ctx context.Context, client *llm.Client, out io.Writer) error {
	models, err := client.Models(ctx)
	if err != nil {
		return err
	}
	for _, model := range models {
		if _, err := fmt.Fprintf(out, "%s\t%q\n", model.ID, model.Name); err != nil {
			return err
		}
	}
	return nil
}
