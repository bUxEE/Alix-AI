package sdkgo

import "github.com/buxee/alix-ai/pkg/contracts/gen/analyst"

// ClientCapabilities is the ADR 0047 announcement sent on turn requests.
type ClientCapabilities = analyst.ClientCapabilities

// DefaultClientCapabilities is what this SDK release can parse of the 1.5
// analyst / rich-block surface. SendTurn includes it unless the caller overrides
// with WithClientCapabilities or clears it with WithoutClientCapabilities.
func DefaultClientCapabilities() ClientCapabilities {
	c := analyst.ClientCapabilitiesContract15
	src := true
	return ClientCapabilities{
		Contract: &c,
		Blocks: []analyst.ClientCapabilitiesBlocksItem{
			analyst.ClientCapabilitiesBlocksItemMarkdown,
			analyst.ClientCapabilitiesBlocksItemCode,
			analyst.ClientCapabilitiesBlocksItemTable,
			analyst.ClientCapabilitiesBlocksItemChart,
			analyst.ClientCapabilitiesBlocksItemDiagram,
			analyst.ClientCapabilitiesBlocksItemCitationList,
			analyst.ClientCapabilitiesBlocksItemActionCard,
			analyst.ClientCapabilitiesBlocksItemClarificationChoices,
			analyst.ClientCapabilitiesBlocksItemConfirmationPanel,
			analyst.ClientCapabilitiesBlocksItemKpi,
			analyst.ClientCapabilitiesBlocksItemTimeline,
			analyst.ClientCapabilitiesBlocksItemRecordCard,
			analyst.ClientCapabilitiesBlocksItemProvenance,
			analyst.ClientCapabilitiesBlocksItemDraft,
		},
		ChartMarks: []analyst.ClientCapabilitiesChartMarksItem{
			analyst.ClientCapabilitiesChartMarksItemBar,
			analyst.ClientCapabilitiesChartMarksItemLine,
			analyst.ClientCapabilitiesChartMarksItemArea,
			analyst.ClientCapabilitiesChartMarksItemPoint,
			analyst.ClientCapabilitiesChartMarksItemPie,
			analyst.ClientCapabilitiesChartMarksItemScatter,
			analyst.ClientCapabilitiesChartMarksItemHeatmap,
			analyst.ClientCapabilitiesChartMarksItemComboBarLine,
		},
		ChartLayouts: []analyst.ClientCapabilitiesChartLayoutsItem{
			analyst.ClientCapabilitiesChartLayoutsItemGrouped,
			analyst.ClientCapabilitiesChartLayoutsItemStacked,
			analyst.ClientCapabilitiesChartLayoutsItemHorizontal,
			analyst.ClientCapabilitiesChartLayoutsItemDonut,
		},
		ProgressStages: []analyst.ClientCapabilitiesProgressStagesItem{
			analyst.ClientCapabilitiesProgressStagesItemInterpreting,
			analyst.ClientCapabilitiesProgressStagesItemRetrieving,
			analyst.ClientCapabilitiesProgressStagesItemSources,
			analyst.ClientCapabilitiesProgressStagesItemValidating,
			analyst.ClientCapabilitiesProgressStagesItemPreparing,
			analyst.ClientCapabilitiesProgressStagesItemCommitting,
			analyst.ClientCapabilitiesProgressStagesItemToolCall,
			analyst.ClientCapabilitiesProgressStagesItemQueued,
			analyst.ClientCapabilitiesProgressStagesItemGenerating,
			analyst.ClientCapabilitiesProgressStagesItemVerifying,
			analyst.ClientCapabilitiesProgressStagesItemAnalystRead,
			analyst.ClientCapabilitiesProgressStagesItemAnalystCompute,
			analyst.ClientCapabilitiesProgressStagesItemAnalystSearch,
			analyst.ClientCapabilitiesProgressStagesItemAnalystFetch,
			analyst.ClientCapabilitiesProgressStagesItemAnalystAnswer,
		},
		SourceLabels: &src,
		BlockFields: []analyst.ClientCapabilitiesBlockFieldsItem{
			analyst.ClientCapabilitiesBlockFieldsItemDatasetID,
			analyst.ClientCapabilitiesBlockFieldsItemBarLayout,
			analyst.ClientCapabilitiesBlockFieldsItemPieStyle,
			analyst.ClientCapabilitiesBlockFieldsItemSource,
		},
	}
}
