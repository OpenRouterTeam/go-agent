package agent

import "github.com/OpenRouterTeam/go-sdk/models/components"

type (
	BaseInputsUnion                   = components.InputsUnion
	ChatAssistantMessage              = components.ChatAssistantMessage
	ChatMessages                      = components.ChatMessages
	EasyInputMessage                  = components.EasyInputMessage
	EasyInputMessageContentInputImage = components.EasyInputMessageContentInputImage
	EasyInputMessageContentUnion1     = components.EasyInputMessageContentUnion1
	EasyInputMessageRoleUnion         = components.EasyInputMessageRoleUnion
	ErrorEvent                        = components.ErrorEvent
	FunctionCallItem                  = components.FunctionCallItem
	FunctionCallOutputItem            = components.FunctionCallOutputItem
	InputAudio                        = components.InputAudio
	InputFile                         = components.InputFile
	InputImage                        = components.InputImage
	InputMessageItem                  = components.InputMessageItem
	InputsUnion                       = components.InputsUnion
	InputText                         = components.InputText
	InputVideo                        = components.InputVideo
	OpenAIResponsesToolChoiceUnion    = components.OpenAIResponsesToolChoiceUnion
	OpenResponsesResult               = components.OpenResponsesResult
	OutputFileSearchCallItem          = components.OutputFileSearchCallItem
	OutputFunctionCallItem            = components.OutputFunctionCallItem
	OutputImage                       = components.OutputImage
	OutputImageGenerationCallItem     = components.OutputImageGenerationCallItem
	OutputItems                       = components.OutputItems
	OutputMessage                     = components.OutputMessageItem
	OutputReasoningItem               = components.OutputReasoningItem
	OutputWebSearchCallItem           = components.OutputWebSearchCallItem
	ResponseOutputText                = components.ResponseOutputText
	ResponsesRequest                  = components.ResponsesRequest
	ResponsesRequestToolUnion         = components.ResponsesRequestToolUnion
	StreamEvents                      = components.StreamEvents
	Usage                             = components.Usage
)

type Item = components.OutputItems
type AssistantMessageItem = components.OutputMessageItem
type CallFunctionToolItem = components.OutputFunctionCallItem
type CallFileSearchItem = components.OutputFileSearchCallItem
type CallImageGenerationItem = components.OutputImageGenerationCallItem
type CallWebSearchItem = components.OutputWebSearchCallItem
type ReasoningItem = components.OutputReasoningItem
type UserMessageItem = components.EasyInputMessage
type DeveloperMessageItem = components.EasyInputMessage
type SystemMessageItem = components.EasyInputMessage
type NewUserMessageItem = components.EasyInputMessage
type ErrorItem = components.ErrorEvent
type FunctionProgressItem = ToolPreliminaryResultEvent
type FunctionResultItem = ToolResultEvent
