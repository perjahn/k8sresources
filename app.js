(() => {
  const resourceKindSelect = document.querySelector("#resource-kind");
  const hideEmptyCheckbox = document.querySelector("#hide-empty");
  const checkboxes = document.querySelectorAll(".include-in-diff");
  const updateRows = () => {
    const selected = new Set(
      Array.from(checkboxes)
        .filter((checkbox) => checkbox.checked)
        .map((checkbox) => checkbox.dataset.clusterIndex)
    );
    document.querySelectorAll("tbody tr[data-diff-row]").forEach((row) => {
      const states = Array.from(row.querySelectorAll("td[data-state]"))
        .filter((cell) => selected.has(cell.dataset.clusterIndex))
        .map((cell) => cell.dataset.state);
      const hasDifference = states.includes("present") && states.includes("missing");
      row.classList.toggle("different", hasDifference);
    });
  };

  const updateEmptyRows = () => {
    if (!hideEmptyCheckbox) return;
    const selectedIndices = new Set(
      Array.from(checkboxes)
        .filter((checkbox) => checkbox.checked)
        .map((checkbox) => checkbox.dataset.clusterIndex)
    );
    const useAllClusters = selectedIndices.size === 0;
    document.querySelectorAll("tbody tr[data-diff-row]").forEach((row) => {
      const cells = Array.from(row.querySelectorAll("td[data-state]"))
        .filter((cell) => useAllClusters || selectedIndices.has(cell.dataset.clusterIndex));
      const isEmpty = cells.length > 0 && cells.every((cell) => {
        if (cell.dataset.state === "missing") return true;
        return cell.dataset.state === "present" && cell.textContent.trim() === "0";
      });
      row.hidden = hideEmptyCheckbox.checked && isEmpty;
    });
  };

  const selectedClusterNames = () =>
    Array.from(checkboxes)
      .filter((checkbox) => checkbox.checked)
      .map((checkbox) => checkbox.dataset.clusterName);

  const clusterParameter = (selectedNames) =>
    selectedNames.length === checkboxes.length
      ? ""
      : `clusters=${selectedNames.map(encodeURIComponent).join(",")}`;

  const updateCurrentURL = (url) => {
    url.searchParams.delete("cluster");
    url.searchParams.delete("cluster_filter");
    url.searchParams.delete("clusters");
    if (url.searchParams.get("kind") === "pod") {
      url.searchParams.delete("kind");
    }
    if (url.searchParams.get("hide_empty") !== "1") {
      url.searchParams.delete("hide_empty");
    }
    const query = url.searchParams.toString();
    const clusters = clusterParameter(selectedClusterNames());
    url.search = [query, clusters].filter(Boolean).join("&");
    window.history.replaceState(null, "", url.pathname + url.search + url.hash);
  };

  const updateClusterSelectionURL = () => {
    updateCurrentURL(new URL(window.location.href));
  };

  const updateHideEmptyURL = () => {
    if (!hideEmptyCheckbox) return;
    const url = new URL(window.location.href);
    if (hideEmptyCheckbox.checked) {
      url.searchParams.set("hide_empty", "1");
    } else {
      url.searchParams.delete("hide_empty");
    }
    updateCurrentURL(url);
  };

  checkboxes.forEach((checkbox) => {
    checkbox.addEventListener("change", () => {
      updateRows();
      updateEmptyRows();
      updateClusterSelectionURL();
    });
  });

  if (hideEmptyCheckbox) {
    hideEmptyCheckbox.addEventListener("change", () => {
      updateEmptyRows();
      updateHideEmptyURL();
    });
  }

  if (resourceKindSelect) {
    if (new URLSearchParams(window.location.search).has("kind")) {
      resourceKindSelect.focus();
    }
    const form = resourceKindSelect.form;
    form.addEventListener("submit", (event) => {
      event.preventDefault();
      form.querySelectorAll("[data-cluster-selection-input]").forEach((input) => input.remove());
      const selectedNames = selectedClusterNames();
      const params = new URLSearchParams();
      new FormData(form).forEach((value, name) => {
        if (typeof value === "string" && name !== "clusters") params.append(name, value);
      });
      if (resourceKindSelect.value === "pod") {
        params.delete("kind");
      }
      const url = new URL(form.action, window.location.href);
      const query = params.toString();
      const clusters = clusterParameter(selectedNames);
      url.search = [query, clusters].filter(Boolean).join("&");
      window.location.assign(url.href);
    });
    resourceKindSelect.addEventListener("change", () => form.requestSubmit());
  }

  updateRows();
  updateEmptyRows();
  updateCurrentURL(new URL(window.location.href));
})();
