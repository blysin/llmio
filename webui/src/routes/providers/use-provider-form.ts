import { useEffect, useRef, useState } from "react";
import { zodResolver } from "@hookform/resolvers/zod";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { createProvider, updateProvider } from "@/lib/api";
import type { Provider, ProviderTemplate } from "@/lib/api";
import { toast } from "sonner";
import {
  CATEGORY_DEFAULT_BASE_URL,
  mergeTemplateWithConfig,
  parseConfigJson,
  stringifyConfigFields,
  type ConfigFieldMap,
  getTemplateInitialConfig,
} from "./provider-form-utils";

export const providerFormSchema = z.object({
  name: z.string().min(1, { message: "提供商名称不能为空" }),
  type: z.string().min(1, { message: "提供商类型不能为空" }),
  category: z.string().min(1, { message: "提供商分类不能为空" }),
  config: z.string().min(1, { message: "配置不能为空" }),
  console: z.string().optional(),
  proxy: z.string().optional(),
  error_matcher: z.string().optional(),
});

export type ProviderFormValues = z.infer<typeof providerFormSchema>;

// 分类默认「其他」：不影响上游协议，只是不展示用量等增强信息
const DEFAULT_CATEGORY = "other";

const defaultFormValues: ProviderFormValues = {
  name: "",
  type: "",
  category: DEFAULT_CATEGORY,
  config: "",
  console: "",
  proxy: "",
  error_matcher: "",
};

type UseProviderFormParams = {
  providerTemplates: ProviderTemplate[];
  refreshProviders: () => Promise<void> | void;
};

export const useProviderForm = ({
  providerTemplates,
  refreshProviders,
}: UseProviderFormParams) => {
  const [open, setOpen] = useState(false);
  const [editingProvider, setEditingProvider] = useState<Provider | null>(null);
  const [configFields, setConfigFields] = useState<ConfigFieldMap>({});
  const [structuredConfigEnabled, setStructuredConfigEnabled] = useState(false);
  const configCacheRef = useRef<Record<string, ConfigFieldMap>>({});

  const form = useForm<ProviderFormValues>({
    resolver: zodResolver(providerFormSchema),
    defaultValues: defaultFormValues,
  });

  const selectedProviderType = form.watch("type");

  useEffect(() => {
    if (!open) {
      setStructuredConfigEnabled(false);
      setConfigFields({});
      configCacheRef.current = {};
      return;
    }

    if (!selectedProviderType) {
      setStructuredConfigEnabled(false);
      setConfigFields({});
      return;
    }

    const template = providerTemplates.find(
      (item) => item.type === selectedProviderType
    );

    if (!template) {
      setStructuredConfigEnabled(false);
      setConfigFields({});
      return;
    }

    const templateFields = parseConfigJson(template.template);
    if (!templateFields) {
      setStructuredConfigEnabled(false);
      setConfigFields({});
      return;
    }

    let nextFields = configCacheRef.current[selectedProviderType];

    if (!nextFields && editingProvider && editingProvider.Type === selectedProviderType) {
      const editingConfig = parseConfigJson(editingProvider.Config);
      if (editingConfig) {
        nextFields = mergeTemplateWithConfig(templateFields, editingConfig, true);
      }
    }

    if (!nextFields) {
      nextFields = templateFields;
    }

    configCacheRef.current[selectedProviderType] = nextFields;
    setConfigFields(nextFields);
    setStructuredConfigEnabled(true);
    form.setValue("config", stringifyConfigFields(nextFields));
  }, [open, selectedProviderType, providerTemplates, editingProvider, form]);

  const handleConfigFieldChange = (key: string, value: string) => {
    setConfigFields((prev) => {
      const updatedFields = { ...prev, [key]: value };
      if (selectedProviderType) {
        configCacheRef.current[selectedProviderType] = updatedFields;
      }
      form.setValue("config", stringifyConfigFields(updatedFields), {
        shouldDirty: true,
        shouldValidate: true,
      });
      return updatedFields;
    });
  };

  // 切换分类时把该渠道的默认 base_url 预填进配置。
  // 仅在结构化配置可用、且当前值是「空」或「某个已知默认值」（其它分类的预设、当前类型的模板默认值）
  // 时才覆盖；用户手填过的地址一律保留，避免把人家的配置冲掉。
  const handleCategoryChange = (category: string) => {
    if (!structuredConfigEnabled) {
      return;
    }
    const presetURL = CATEGORY_DEFAULT_BASE_URL[category];
    if (!presetURL) {
      return;
    }

    const knownDefaults = new Set(Object.values(CATEGORY_DEFAULT_BASE_URL));
    const template = providerTemplates.find((item) => item.type === selectedProviderType);
    const templateBaseURL = template ? parseConfigJson(template.template)?.base_url : undefined;
    if (templateBaseURL) {
      knownDefaults.add(templateBaseURL);
    }

    const currentURL = configFields.base_url ?? "";
    if (currentURL && !knownDefaults.has(currentURL)) {
      return;
    }

    handleConfigFieldChange("base_url", presetURL);
  };

  const openEditDialog = (provider: Provider) => {
    configCacheRef.current = {};
    setEditingProvider(provider);
    form.reset({
      name: provider.Name,
      type: provider.Type,
      category: provider.Category || DEFAULT_CATEGORY,
      config: provider.Config,
      console: provider.Console || "",
      proxy: provider.Proxy || "",
      error_matcher: provider.ErrorMatcher || "",
    });
    setOpen(true);
  };

  const openCreateDialog = () => {
    configCacheRef.current = {};

    if (providerTemplates.length === 0) {
      toast.error("暂无可用的提供商模板");
      return;
    }

    setEditingProvider(null);
    const firstTemplate = providerTemplates[0];
    form.reset({
      name: "",
      type: firstTemplate?.type ?? "",
      category: DEFAULT_CATEGORY,
      config: getTemplateInitialConfig(firstTemplate),
      console: "",
      proxy: "",
      error_matcher: "",
    });
    setOpen(true);
  };

  const submit = async (values: ProviderFormValues) => {
    try {
      if (editingProvider) {
        await updateProvider(editingProvider.ID, {
          name: values.name,
          type: values.type,
          category: values.category,
          config: values.config,
          console: values.console || "",
          proxy: values.proxy || "",
          error_matcher: values.error_matcher || "",
        });
        toast.success(`提供商 ${values.name} 更新成功`);
        setEditingProvider(null);
      } else {
        await createProvider({
          name: values.name,
          type: values.type,
          category: values.category,
          config: values.config,
          console: values.console || "",
          proxy: values.proxy || "",
          error_matcher: values.error_matcher || "",
        });
        toast.success(`提供商 ${values.name} 创建成功`);
      }

      form.reset(defaultFormValues);
      setOpen(false);
      await refreshProviders();
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      toast.error(`${editingProvider ? "更新" : "创建"}提供商失败: ${message}`);
      console.error(err);
    }
  };

  return {
    form,
    open,
    setOpen,
    editingProvider,
    structuredConfigEnabled,
    configFields,
    openEditDialog,
    openCreateDialog,
    handleConfigFieldChange,
    handleCategoryChange,
    submit,
  };
};
